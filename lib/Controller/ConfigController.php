<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil\Controller;

use OCA\TaskProcUtil\AppConfig;
use OCA\TaskProcUtil\Settings\AdminSettings;
use OCP\AppFramework\Http;
use OCP\AppFramework\Http\Attribute\ApiRoute;
use OCP\AppFramework\Http\Attribute\AuthorizedAdminSetting;
use OCP\AppFramework\Http\Attribute\NoCSRFRequired;
use OCP\AppFramework\Http\DataResponse;
use OCP\AppFramework\OCSController;
use OCP\IRequest;

/**
 * Admin-only config endpoints.
 *
 * - The admin settings UI uses these to read and persist the supervisor config.
 * - The Go supervisor reads the config once on start and after each poll so it
 *   picks up changes made in the UI without a restart.
 *
 * @psalm-suppress UnusedClass
 */
class ConfigController extends OCSController {
	public function __construct(
		string $appName,
		IRequest $request,
		private AppConfig $config,
	) {
		parent::__construct($appName, $request);
	}

	/**
	 * Get the current supervisor configuration.
	 *
	 * @return DataResponse<Http::STATUS_OK, array{max_workers: int, poll_interval: int, enabled: bool}, array{}>
	 *
	 * 200: Configuration returned
	 */
	#[NoCSRFRequired]
	#[AuthorizedAdminSetting(settings: AdminSettings::class)]
	#[ApiRoute(verb: 'GET', url: '/config', root: '/task_proc_util')]
	public function getConfig(): DataResponse {
		return new DataResponse($this->config->toArray());
	}

	/**
	 * Update the supervisor configuration.
	 *
	 * @param int|null $maxWorkers Maximum number of workers allowed
	 * @param int|null $pollInterval Seconds between queue polls
	 * @param bool|null $enabled Whether the supervisor should run
	 * @return DataResponse<Http::STATUS_OK, array{max_workers: int, poll_interval: int, enabled: bool}, array{}>|DataResponse<Http::STATUS_BAD_REQUEST, array{message: string}, array{}>
	 *
	 * 200: Configuration saved
	 * 400: Invalid configuration
	 */
	#[AuthorizedAdminSetting(settings: AdminSettings::class)]
	#[ApiRoute(verb: 'PUT', url: '/config', root: '/task_proc_util')]
	public function setConfig(
		?int $maxWorkers = null,
		?int $pollInterval = null,
		?bool $enabled = null,
	): DataResponse {
		if ($maxWorkers !== null && $maxWorkers < 1) {
			return new DataResponse(
				['message' => 'Maximum workers must be at least 1'],
				Http::STATUS_BAD_REQUEST,
			);
		}

		if ($maxWorkers !== null) {
			$this->config->setMaxWorkers($maxWorkers);
		}
		if ($pollInterval !== null) {
			$this->config->setPollInterval($pollInterval);
		}
		if ($enabled !== null) {
			$this->config->setEnabled($enabled);
		}

		return new DataResponse($this->config->toArray());
	}
}
