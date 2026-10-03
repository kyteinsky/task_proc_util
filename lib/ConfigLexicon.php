<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil;

use OCP\Config\Lexicon\Entry;
use OCP\Config\Lexicon\ILexicon;
use OCP\Config\Lexicon\Strictness;
use OCP\Config\ValueType;

/**
 * Declares every app config key this app owns, with its type, default and
 * description.
 *
 * The defaults here are authoritative: the lexicon default overrides the one
 * passed to getValueInt() and friends. AppConfig therefore reads its defaults
 * from this class rather than repeating them.
 *
 * The Go supervisor reads the same keys straight from oc_appconfig and carries
 * its own copies of these defaults in db.DefaultConfig(), because it cannot
 * call into PHP. The two must stay in agreement.
 *
 * @psalm-suppress UnusedClass registered via Application::register()
 */
class ConfigLexicon implements ILexicon {
	public const MAX_WORKERS = 'max_workers';
	public const POLL_INTERVAL = 'poll_interval';
	public const CONFIG_INTERVAL = 'config_interval';

	/**
	 * Storage key for the auto-scaler switch.
	 *
	 * NOT 'enabled': that key is reserved by Nextcloud itself to record whether
	 * an app is enabled ('yes' / 'no' / a JSON list of group ids), written by
	 * AppManager and read as a string. Storing our own boolean there makes
	 * `occ config:list` throw AppConfigTypeConflictException and corrupts the
	 * app's own enable state.
	 */
	public const ENABLED = 'autoscale_enabled';

	public const DEFAULT_MAX_WORKERS = 4;
	public const DEFAULT_POLL_INTERVAL = 10;
	public const DEFAULT_CONFIG_INTERVAL = 300;
	public const DEFAULT_ENABLED = true;

	/**
	 * Report unknown keys but keep serving them.
	 *
	 * Not WARNING or EXCEPTION: those block reads and writes of unlisted keys.
	 * This app's own keys are all listed below, so blocking would only ever
	 * hit a key added in a future version against an older lexicon, and
	 * silently returning a default there is harder to diagnose than a log line.
	 */
	#[\Override]
	public function getStrictness(): Strictness {
		return Strictness::NOTICE;
	}

	/**
	 * @return Entry[]
	 */
	#[\Override]
	public function getAppConfigs(): array {
		return [
			new Entry(
				key: self::MAX_WORKERS,
				type: ValueType::INT,
				defaultRaw: self::DEFAULT_MAX_WORKERS,
				definition: 'Maximum number of concurrent workers across all task types',
			),
			new Entry(
				key: self::POLL_INTERVAL,
				type: ValueType::INT,
				defaultRaw: self::DEFAULT_POLL_INTERVAL,
				definition: 'Seconds between queue polls, when the supervisor rescales workers',
			),
			new Entry(
				key: self::CONFIG_INTERVAL,
				type: ValueType::INT,
				defaultRaw: self::DEFAULT_CONFIG_INTERVAL,
				definition: 'Seconds between supervisor re-reads of these settings. '
					. 'A change takes at most this long to take effect.',
			),
			new Entry(
				key: self::ENABLED,
				type: ValueType::BOOL,
				defaultRaw: self::DEFAULT_ENABLED,
				definition: 'Whether the auto-scaler runs. When off, all workers are stopped.',
			),
		];
	}

	/**
	 * This app stores no per-user configuration.
	 *
	 * @return Entry[]
	 */
	#[\Override]
	public function getUserConfigs(): array {
		return [];
	}
}
