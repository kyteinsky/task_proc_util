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
 * These values are written by the admin settings UI. The Go supervisor reads
 * them straight from the database on its own schedule.
 *
 * @see ConfigLexicon for the keys, types and defaults these accessors use.
 */
class AppConfig implements \JsonSerializable {
	/** Public API field name, kept stable for the admin UI and the OCS shape. */
	public const FIELD_ENABLED = 'enabled';

	public function __construct(
		private IAppConfig $appConfig,
	) {
	}

	public function getMaxWorkers(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, ConfigLexicon::MAX_WORKERS, ConfigLexicon::DEFAULT_MAX_WORKERS);
	}

	public function setMaxWorkers(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, ConfigLexicon::MAX_WORKERS, max(1, $value));
	}

	public function getPollInterval(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, ConfigLexicon::POLL_INTERVAL, ConfigLexicon::DEFAULT_POLL_INTERVAL);
	}

	public function setPollInterval(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, ConfigLexicon::POLL_INTERVAL, max(1, $value));
	}

	/**
	 * Seconds between re-reads of this config by the supervisor.
	 *
	 * Deliberately much coarser than the poll interval: the poll loop hits the
	 * task tables every few seconds, but admin settings change rarely, so
	 * re-reading them at the same rate is pure overhead.
	 */
	public function getConfigInterval(): int {
		return $this->appConfig->getValueInt(Application::APP_ID, ConfigLexicon::CONFIG_INTERVAL, ConfigLexicon::DEFAULT_CONFIG_INTERVAL);
	}

	public function setConfigInterval(int $value): void {
		$this->appConfig->setValueInt(Application::APP_ID, ConfigLexicon::CONFIG_INTERVAL, max(1, $value));
	}

	public function isEnabled(): bool {
		return $this->appConfig->getValueBool(Application::APP_ID, ConfigLexicon::ENABLED, ConfigLexicon::DEFAULT_ENABLED);
	}

	public function setEnabled(bool $value): void {
		$this->appConfig->setValueBool(Application::APP_ID, ConfigLexicon::ENABLED, $value);
	}

	/**
	 * @return array{max_workers: int, poll_interval: int, config_interval: int, enabled: bool}
	 */
	public function jsonSerialize(): array {
		return [
			ConfigLexicon::MAX_WORKERS => $this->getMaxWorkers(),
			ConfigLexicon::POLL_INTERVAL => $this->getPollInterval(),
			ConfigLexicon::CONFIG_INTERVAL => $this->getConfigInterval(),
			self::FIELD_ENABLED => $this->isEnabled(),
		];
	}
}
