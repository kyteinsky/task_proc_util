<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\TaskProcUtil\Settings;

use OCA\TaskProcUtil\AppConfig;
use OCA\TaskProcUtil\AppInfo\Application;
use OCP\AppFramework\Http\TemplateResponse;
use OCP\AppFramework\Services\IInitialState;
use OCP\IL10N;
use OCP\Settings\IDelegatedSettings;

class AdminSettings implements IDelegatedSettings {
	public function __construct(
		private IInitialState $initialState,
		private AppConfig $config,
		private IL10N $l,
	) {
	}

	#[\Override]
	public function getForm(): TemplateResponse {
		$this->initialState->provideInitialState('config', $this->config->toArray());
		return new TemplateResponse(Application::APP_ID, 'adminSettings');
	}

	#[\Override]
	public function getSection(): string {
		return 'ai';
	}

	#[\Override]
	public function getPriority(): int {
		return 50;
	}

	#[\Override]
	public function getName(): ?string {
		return $this->l->t('Task processing workers');
	}

	#[\Override]
	public function getAuthorizedAppConfig(): array {
		return []; // Handled by ConfigController
	}
}
