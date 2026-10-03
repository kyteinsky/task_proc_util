<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil;

use OCA\TaskProcUtil\AppInfo\Application;
use OCP\IAppConfig;

/**
 * Typed accessor for the supervisor configuration stored in app config.
 *
 * These values are written by the admin settings UI and read by the Go
 * supervisor through the config controller.
 */
class AppConfig implements \JsonSerializable {
	public const KEY_MAX_WORKERS = 'max_workers';
	public const KEY_POLL_INTERVAL = 'poll_interval';
	public const KEY_CONFIG_INTERVAL = 'config_interval';

	/**
	 * Storage key for the auto-scaler switch.
	 *
	 * NOT 'enabled': that key is reserved by Nextcloud itself to record whether
	 * an app is enabled ('yes' / 'no' / a JSON list of group ids), written by
	 * AppManager and read as a string. Storing our own boolean there makes
	 * `occ config:list` throw AppConfigTypeConflictException and corrupts the
	 * app's own enable state.
	 */
	public const KEY_ENABLED = 'autoscale_enabled';

	/** Public API field name, kept stable for the admin UI and the OCS shape. */
	public const FIELD_ENABLED = 'enabled';

	private const DEFAULT_MAX_WORKERS = 4;
	private const DEFAULT_POLL_INTERVAL = 10;
	private const DEFAULT_CONFIG_INTERVAL = 300;

	public function __construct(
		private IAppConfig $appConfig,
	) {
	}

	public function getMaxWorkers(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, self::KEY_MAX_WORKERS, self::DEFAULT_MAX_WORKERS);
	}

	public function setMaxWorkers(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, self::KEY_MAX_WORKERS, max(1, $value));
	}

	public function getPollInterval(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, self::KEY_POLL_INTERVAL, self::DEFAULT_POLL_INTERVAL);
	}

	public function setPollInterval(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, self::KEY_POLL_INTERVAL, max(1, $value));
	}

	/**
	 * Seconds between re-reads of this config by the supervisor.
	 *
	 * Deliberately much coarser than the poll interval: the poll loop hits the
	 * task tables every few seconds, but admin settings change rarely, so
	 * re-reading them at the same rate is pure overhead.
	 */
	public function getConfigInterval(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, self::KEY_CONFIG_INTERVAL, self::DEFAULT_CONFIG_INTERVAL);
	}

	public function setConfigInterval(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, self::KEY_CONFIG_INTERVAL, max(1, $value));
	}

	public function isEnabled(): bool {
		return $this->appConfig->getValueBool(Application::APP_ID, self::KEY_ENABLED, true);
	}

	public function setEnabled(bool $value): void {
		$this->appConfig->setValueBool(Application::APP_ID, self::KEY_ENABLED, $value);
	}

	/**
	 * @return array{max_workers: int, poll_interval: int, config_interval: int, enabled: bool}
	 */
	public function jsonSerialize(): array {
		return [
			self::KEY_MAX_WORKERS => $this->getMaxWorkers(),
			self::KEY_POLL_INTERVAL => $this->getPollInterval(),
			self::KEY_CONFIG_INTERVAL => $this->getConfigInterval(),
			self::FIELD_ENABLED => $this->isEnabled(),
		];
	}
}
