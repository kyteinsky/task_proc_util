<!--
  - SPDX-FileCopyrightText: 2026 Nextcloud contributors
  - SPDX-License-Identifier: AGPL-3.0-or-later
-->
<template>
	<NcSettingsSection
		:name="t('task_proc_util', 'Task Processing auto-scaler')"
		:description="t('task_proc_util', 'Automatically scale the number of Task Processing workers based on the size of the task queue. Tasks of every type are scheduled fairly so a flood of one type cannot starve the others.')">
		<div class="tpu-settings">
			<NcCheckboxRadioSwitch
				:model-value="config.enabled"
				type="switch"
				@update:model-value="onToggleEnabled">
				{{ t('task_proc_util', 'Enable the auto-scaler') }}
			</NcCheckboxRadioSwitch>

			<div class="tpu-field">
				<NcTextField
					v-model="maxWorkersStr"
					type="number"
					:label="t('task_proc_util', 'Maximum workers')"
					:helper-text="t('task_proc_util', 'Upper bound on concurrent workers across all task types')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<div class="tpu-field">
				<NcTextField
					v-model="pollIntervalStr"
					type="number"
					:label="t('task_proc_util', 'Poll interval (seconds)')"
					:helper-text="t('task_proc_util', 'How often the supervisor checks the queue and rescales workers')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<div class="tpu-field">
				<NcTextField
					v-model="configIntervalStr"
					type="number"
					:label="t('task_proc_util', 'Config reload interval (seconds)')"
					:helper-text="t('task_proc_util', 'How often the supervisor re-reads these settings. Changes made here take up to this long to take effect.')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<div v-if="loading" class="tpu-saving-info">
				<NcLoadingIcon :size="20" class="icon" />
				{{ t('task_proc_util', 'Saving…') }}
			</div>
			<div v-if="error" class="tpu-error">
				{{ error }}
			</div>
		</div>
	</NcSettingsSection>
</template>

<script>
import axios from '@nextcloud/axios'
import { showError, showSuccess } from '@nextcloud/dialogs'
import { loadState } from '@nextcloud/initial-state'
import { translate as t } from '@nextcloud/l10n'
import { generateOcsUrl } from '@nextcloud/router'
import NcCheckboxRadioSwitch from '@nextcloud/vue/components/NcCheckboxRadioSwitch'
import NcLoadingIcon from '@nextcloud/vue/components/NcLoadingIcon'
import NcSettingsSection from '@nextcloud/vue/components/NcSettingsSection'
import NcTextField from '@nextcloud/vue/components/NcTextField'

let timeout
/**
 * Returns a debounced version of `fn` that delays invocation until `ms`
 * milliseconds have elapsed since the last call.
 *
 * @param {(...args: unknown[]) => void} fn the function to debounce
 * @param {number} ms the debounce delay in milliseconds
 * @return {(...args: unknown[]) => void} the debounced function
 */
function debounce(fn, ms = 2000) {
	return function(...args) {
		clearTimeout(timeout)
		timeout = setTimeout(() => fn.apply(this, args), ms)
	}
}

export default {
	name: 'AdminSettings',

	components: {
		NcSettingsSection,
		NcTextField,
		NcCheckboxRadioSwitch,
		NcLoadingIcon,
	},

	data() {
		const config = loadState('task_proc_util', 'config', {
			max_workers: 4,
			poll_interval: 10,
			config_interval: 300,
			enabled: true,
		})
		return {
			config,
			maxWorkersStr: String(config.max_workers),
			pollIntervalStr: String(config.poll_interval),
			configIntervalStr: String(config.config_interval),
			error: '',
			loading: false,
		}
	},

	methods: {
		onChange() {
			this.error = ''
			debounce(this.save)()
		},

		onToggleEnabled(value) {
			this.config.enabled = value
			this.save()
		},

		validate() {
			const max = parseInt(this.maxWorkersStr, 10)
			const poll = parseInt(this.pollIntervalStr, 10)
			const configInterval = parseInt(this.configIntervalStr, 10)
			if (Number.isNaN(max) || Number.isNaN(poll) || Number.isNaN(configInterval)) {
				return { ok: false, message: t('task_proc_util', 'All values must be numbers') }
			}
			if (max < 1 || poll < 1 || configInterval < 1) {
				return { ok: false, message: t('task_proc_util', 'Values are out of range') }
			}
			return { ok: true, max, poll, configInterval }
		},

		async save() {
			const result = this.validate()
			if (!result.ok) {
				this.error = result.message
				return
			}
			try {
				this.loading = true
				const { data } = await axios.put(
					generateOcsUrl('task_proc_util/config'),
					{
						maxWorkers: result.max,
						pollInterval: result.poll,
						configInterval: result.configInterval,
						enabled: this.config.enabled,
					},
				)
				this.config = data.ocs.data
				this.maxWorkersStr = String(this.config.max_workers)
				this.pollIntervalStr = String(this.config.poll_interval)
				this.configIntervalStr = String(this.config.config_interval)
				showSuccess(t('task_proc_util', 'Settings saved'))
			} catch (e) {
				this.error = e.response?.data?.ocs?.data?.message
					|| e.response?.data?.message
					|| t('task_proc_util', 'Failed to save settings')
				showError(this.error)
			} finally {
				this.loading = false
			}
		},
	},
}
</script>

<style scoped lang="scss">
.tpu-settings {
	display: flex;
	flex-direction: column;
	gap: 16px;
	max-width: 480px;

	.tpu-field {
		max-width: 320px;

		:deep(.input-field__helper-text-message) {
			width: 480px;
			max-width: 480px;
		}
	}

	.tpu-saving-info {
		display: flex;
		flex-direction: row;
	}

	.tpu-error {
		color: var(--color-error);
	}
}
</style>
