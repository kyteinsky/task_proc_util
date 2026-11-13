<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil\Migration;

use OCA\TaskProcUtil\AppInfo\Application;
use OCP\App\IAppManager;
use OCP\Migration\IOutput;
use OCP\Migration\IRepairStep;

/**
 * Restores the executable bit on the shipped supervisor wrapper and binaries.
 *
 * The release tarball stores them as 0755, but the server's app installer
 * copies every extracted file into apps/ with PHP's copy(), which does not
 * preserve permissions. After an app store install or update the files are
 * therefore 0644 and systemd fails with "Permission denied".
 *
 * Runs on install and after every upgrade, since each update replaces the
 * files again.
 */
class MakeBinariesExecutable implements IRepairStep {
	/** Paths relative to the app root, wrapper first. */
	private const BINARIES = [
		'bin/task_proc_util-supervisor',
		'bin/amd64/task_proc_util-supervisor',
		'bin/arm64/task_proc_util-supervisor',
		'bin/arm/task_proc_util-supervisor',
	];

	public function __construct(
		private readonly IAppManager $appManager,
	) {
	}

	#[\Override]
	public function getName(): string {
		return 'Make the supervisor binaries executable';
	}

	#[\Override]
	public function run(IOutput $output): void {
		$appPath = $this->appManager->getAppPath(Application::APP_ID);

		foreach (self::BINARIES as $relative) {
			$path = rtrim($appPath, '/') . '/' . $relative;
			// A dev checkout may hold only the host-arch binary.
			if (!is_file($path)) {
				continue;
			}
			// Already executable (git checkout, manual install): skip the write
			// so app dirs not owned by the web server user do not warn needlessly.
			$mode = fileperms($path) & 0777;
			if (($mode & 0111) === 0111) {
				continue;
			}
			if (!@chmod($path, $mode | 0111)) {
				$output->warning('Could not make ' . $path . ' executable. Run: chmod +x ' . escapeshellarg($path));
			}
		}
	}
}
