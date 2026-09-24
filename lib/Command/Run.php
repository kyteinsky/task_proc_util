<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil\Command;

use OCP\AppFramework\Utility\ITimeFactory;
use OCP\TaskProcessing\Exception\Exception;
use OCP\TaskProcessing\Exception\NotFoundException;
use OCP\TaskProcessing\IManager;
use OCP\TaskProcessing\ISynchronousProvider;
use OCP\TaskProcessing\Task;
use Psr\Log\LoggerInterface;
use Symfony\Component\Console\Command\Command;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Input\InputOption;
use Symfony\Component\Console\Output\ConsoleOutputInterface;
use Symfony\Component\Console\Output\OutputInterface;

/**
 * A long-lived worker pinned to a single task type, managed by the Go supervisor.
 *
 * The supervisor (not this command) decides how many workers run per task type
 * at any moment, scaling the pool between the admin-configured min and max based
 * on the queue. Each worker bootstraps Nextcloud once and then drains tasks of
 * its assigned type until a bound is hit, so it recycles to pick up config
 * changes (mirroring the core `taskprocessing:worker --timeout` guidance).
 *
 * Compared to the core worker this command:
 *   - is pinned to exactly one task type (the supervisor schedules fairness
 *     across types by distributing worker slots),
 *   - atomically locks each task with lockTask() before processing, so that the
 *     many workers the supervisor runs in parallel never double-process a task,
 *   - supports a --max-tasks bound in addition to --timeout for recycling.
 *
 * Exit code is always 0 on a clean exit (timeout, max-tasks, idle-exit, signal).
 */
class Run extends Command {
	/** Set by the SIGTERM/SIGINT handler to request a clean exit after the current task. */
	private bool $stopRequested = false;

	public function __construct(
		private readonly IManager $taskProcessingManager,
		private readonly LoggerInterface $logger,
		private readonly ITimeFactory $timeFactory,
	) {
		parent::__construct();
	}

	#[\Override]
	protected function configure(): void {
		$this
			->setName('taskprocessing-util:run')
			->setDescription('Run a worker pinned to a single task type until a bound is reached, then exit')
			->addOption(
				'taskType',
				't',
				InputOption::VALUE_REQUIRED,
				'The task type ID this worker processes'
			)
			->addOption(
				'timeout',
				null,
				InputOption::VALUE_REQUIRED,
				'Exit after this many seconds (0 = no time limit)',
				'0'
			)
			->addOption(
				'max-tasks',
				null,
				InputOption::VALUE_REQUIRED,
				'Exit after processing this many tasks (0 = no task limit)',
				'0'
			)
			->addOption(
				'interval',
				'i',
				InputOption::VALUE_REQUIRED,
				'Seconds to sleep between polls when no task was available',
				'1'
			)
			->addOption(
				'exit-when-idle',
				null,
				InputOption::VALUE_NONE,
				'Exit immediately when the queue for this task type is empty (used for burst scaling)'
			);
	}

	#[\Override]
	protected function execute(InputInterface $input, OutputInterface $output): int {
		$taskTypeId = $input->getOption('taskType');
		if (!is_string($taskTypeId) || $taskTypeId === '') {
			$output->writeln('<error>--taskType is required</error>');
			return 1;
		}
		$timeout = (int)$input->getOption('timeout');
		$maxTasks = (int)$input->getOption('max-tasks');
		$interval = max(1, (int)$input->getOption('interval'));
		$exitWhenIdle = $input->getOption('exit-when-idle') === true;

		$provider = $this->getPreferredSynchronousProvider($taskTypeId);
		if ($provider === null) {
			// Nothing this worker can ever do: the type has no preferred
			// *synchronous* provider (unset, or served by an async/ExApp
			// provider that is driven elsewhere). Report it on stderr and use a
			// distinct exit code so the supervisor stops respawning us every
			// poll tick instead of silently burning a PHP bootstrap each time.
			$this->errorOutput($output)->writeln(
				'<error>No preferred synchronous provider for task type ' . $taskTypeId
				. '; this worker has nothing to do.</error>'
			);
			return self::EXIT_NO_SYNC_PROVIDER;
		}

		// Handle SIGTERM/SIGINT from the supervisor: finish the task in flight,
		// then exit at the top of the next iteration. Without explicit handlers
		// the default disposition would terminate mid-task and strand it as
		// RUNNING. The supervisor escalates to SIGKILL if we overrun its grace
		// period.
		if (function_exists('pcntl_async_signals')) {
			pcntl_async_signals(true);
			$stop = function () use ($output): void {
				$this->stopRequested = true;
				$output->writeln('Stop requested, finishing current task', OutputInterface::VERBOSITY_VERBOSE);
			};
			pcntl_signal(SIGTERM, $stop);
			pcntl_signal(SIGINT, $stop);
		}

		$startTime = $this->timeFactory->now()->getTimestamp();
		$processedCount = 0;

		while (true) {
			/** @psalm-suppress RedundantCondition */
			if ($this->stopRequested) {
				$output->writeln('Stopping on signal', OutputInterface::VERBOSITY_VERBOSE);
				break;
			}
			if ($timeout > 0 && ($startTime + $timeout) < $this->timeFactory->now()->getTimestamp()) {
				$output->writeln('Timeout reached, exiting', OutputInterface::VERBOSITY_VERBOSE);
				break;
			}
			if ($maxTasks > 0 && $processedCount >= $maxTasks) {
				$output->writeln('Max tasks reached, exiting', OutputInterface::VERBOSITY_VERBOSE);
				break;
			}

			$result = $this->processOne($output, $taskTypeId, $provider);
			if ($result === self::RESULT_PROCESSED) {
				$processedCount++;
				continue;
			}
			if ($result === self::RESULT_ERROR) {
				return 1;
			}

			// No task available.
			if ($exitWhenIdle) {
				$output->writeln('Queue empty, exiting (idle)', OutputInterface::VERBOSITY_VERBOSE);
				break;
			}
			// Sleep in 1s steps so a signal is acted on promptly rather than
			// after the full interval.
			/** @psalm-suppress RedundantCondition */
			for ($slept = 0; $slept < $interval && !$this->stopRequested; $slept++) {
				sleep(1);
			}
		}

		return 0;
	}

	private const RESULT_PROCESSED = 0;
	private const RESULT_NO_TASK = 1;
	private const RESULT_ERROR = 2;

	/**
	 * Exit code for "this task type has no preferred synchronous provider".
	 *
	 * Distinct from a generic failure (1) so the supervisor can recognise a
	 * permanently unworkable task type and stop respawning workers for it.
	 */
	public const EXIT_NO_SYNC_PROVIDER = 3;

	/** Max lock races to lose in one claim before backing off (NC < 35 path only). */
	private const MAX_CLAIM_ATTEMPTS = 10;

	/**
	 * Claim and process a single task of the pinned type.
	 *
	 * Concurrent workers never double-process a task: on NC 35+ the claim is a
	 * single atomic SELECT ... FOR UPDATE SKIP LOCKED, on older versions we fall
	 * back to fetch + lockTask() and skip tasks another worker locked first.
	 */
	private function processOne(OutputInterface $output, string $taskTypeId, ISynchronousProvider $provider): int {
		try {
			$task = $this->claimTask($taskTypeId);
		} catch (Exception $e) {
			$this->logger->error('Error claiming scheduled task for type ' . $taskTypeId, ['exception' => $e]);
			return self::RESULT_ERROR;
		}

		if ($task === null) {
			return self::RESULT_NO_TASK;
		}

		$taskId = (string)$task->getId();
		$output->writeln(
			'Processing task ' . $taskId . ' of type ' . $taskTypeId . ' with provider ' . $provider->getId(),
			OutputInterface::VERBOSITY_VERBOSE
		);
		$this->taskProcessingManager->processTask($task, $provider);
		$output->writeln('Finished task ' . $taskId, OutputInterface::VERBOSITY_VERBOSE);
		return self::RESULT_PROCESSED;
	}

	/**
	 * Claim the oldest scheduled task of the pinned type, marking it RUNNING.
	 *
	 * @return Task|null The claimed task, or null if nothing is schedulable.
	 * @throws Exception If the query failed
	 */
	private function claimTask(string $taskTypeId): ?Task {
		// NC 35+: atomic claim, no lock race and no ignore list needed.
		if (method_exists($this->taskProcessingManager, 'claimNextScheduledTask')) {
			return $this->taskProcessingManager->claimNextScheduledTask([$taskTypeId]);
		}

		// NC 33/34: fetch then lock, retrying past tasks another worker locked first.
		// Bounded: under heavy contention new tasks can be scheduled as fast as we
		// iterate, so give up after a few attempts and let the caller back off
		// rather than busy-spinning (and growing the ignore list unboundedly, which
		// would eventually blow the DB parameter limit).
		/** @var list<int> $ignoreIds */
		$ignoreIds = [];
		for ($attempt = 0; $attempt < self::MAX_CLAIM_ATTEMPTS; $attempt++) {
			try {
				$task = $this->taskProcessingManager->getNextScheduledTask([$taskTypeId], $ignoreIds);
			} catch (NotFoundException) {
				return null;
			}

			if ($this->taskProcessingManager->lockTask($task)) {
				return $task;
			}
			// Lost the race; ignore this one and try the next oldest.
			$taskId = $task->getId();
			if ($taskId === null) {
				// Cannot exclude an unsaved task; back off instead of looping.
				return null;
			}
			$ignoreIds[] = $taskId;
		}

		return null;
	}

	/** Writes to stderr when available so worker diagnostics are not mixed into stdout. */
	private function errorOutput(OutputInterface $output): OutputInterface {
		return $output instanceof ConsoleOutputInterface ? $output->getErrorOutput() : $output;
	}

	private function getPreferredSynchronousProvider(string $taskTypeId): ?ISynchronousProvider {
		try {
			$preferred = $this->taskProcessingManager->getPreferredProvider($taskTypeId);
		} catch (Exception $e) {
			$this->logger->error('Failed to get preferred provider for task type ' . $taskTypeId, ['exception' => $e]);
			return null;
		}
		if (!$preferred instanceof ISynchronousProvider) {
			// A provider exists but is asynchronous (typically an ExApp), which
			// this worker cannot drive. Name it so the admin can tell this case
			// apart from "no provider configured at all".
			$this->logger->debug('Preferred provider for ' . $taskTypeId . ' is not synchronous', [
				'provider' => $preferred->getId(),
			]);
			return null;
		}
		return $preferred;
	}
}
