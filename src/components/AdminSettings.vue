<!--
  - SPDX-FileCopyrightText: 2026 Nextcloud contributors
  - SPDX-License-Identifier: AGPL-3.0-or-later
-->
<template>
	<NcSettingsSection :name="t('task_proc_util', 'Task Processing auto-scaler')"
		:description="t('task_proc_util', 'Automatically scale the number of Task Processing workers based on the size of the task queue. Tasks of every type are scheduled fairly so a flood of one type cannot starve the others.')">
		<div class="tpu-settings">
			<NcCheckboxRadioSwitch :model-value="config.enabled"
				type="switch"
				@update:model-value="onToggleEnabled">
				{{ t('task_proc_util', 'Enable the auto-scaler') }}
			</NcCheckboxRadioSwitch>

			<div class="tpu-field">
				<NcTextField v-model="minWorkersStr"
					type="number"
					:label="t('task_proc_util', 'Minimum workers')"
					:helper-text="t('task_proc_util', 'Workers kept running while there is pending work')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<div class="tpu-field">
				<NcTextField v-model="maxWorkersStr"
					type="number"
					:label="t('task_proc_util', 'Maximum workers')"
					:helper-text="t('task_proc_util', 'Upper bound on concurrent workers across all task types')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<div class="tpu-field">
				<NcTextField v-model="pollIntervalStr"
					type="number"
					:label="t('task_proc_util', 'Poll interval (seconds)')"
					:helper-text="t('task_proc_util', 'How often the supervisor checks the queue')"
					:disabled="!config.enabled"
					@update:model-value="onChange" />
			</div>

			<p v-if="error" class="tpu-error">{{ error }}</p>
		</div>
	</NcSettingsSection>
</template>

<script>
import { loadState } from '@nextcloud/initial-state'
import { generateOcsUrl } from '@nextcloud/router'
import { showError, showSuccess } from '@nextcloud/dialogs'
import { translate as t } from '@nextcloud/l10n'
import axios from '@nextcloud/axios'

import NcSettingsSection from '@nextcloud/vue/components/NcSettingsSection'
import NcTextField from '@nextcloud/vue/components/NcTextField'
import NcCheckboxRadioSwitch from '@nextcloud/vue/components/NcCheckboxRadioSwitch'

/**
 * Returns a debounced version of `fn` that delays invocation until `ms`
 * milliseconds have elapsed since the last call.
 *
 * @param {Function} fn the function to debounce
 * @param {number} ms the debounce delay in milliseconds
 * @return {Function} the debounced function
 */
function debounce(fn, ms = 600) {
	let timeout
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
	},

	data() {
		const config = loadState('task_proc_util', 'config', {
			min_workers: 1,
			max_workers: 4,
			poll_interval: 10,
			enabled: true,
		})
		return {
			config,
			minWorkersStr: String(config.min_workers),
			maxWorkersStr: String(config.max_workers),
			pollIntervalStr: String(config.poll_interval),
			error: '',
		}
	},

	created() {
		this.debouncedSave = debounce(() => this.save(), 600)
	},

	methods: {
		t,

		onChange() {
			this.error = ''
			this.debouncedSave()
		},

		onToggleEnabled(value) {
			this.config.enabled = value
			this.save()
		},

		validate() {
			const min = parseInt(this.minWorkersStr, 10)
			const max = parseInt(this.maxWorkersStr, 10)
			const poll = parseInt(this.pollIntervalStr, 10)
			if (Number.isNaN(min) || Number.isNaN(max) || Number.isNaN(poll)) {
				return { ok: false, message: t('task_proc_util', 'All values must be numbers') }
			}
			if (min < 0 || max < 1 || poll < 1) {
				return { ok: false, message: t('task_proc_util', 'Values are out of range') }
			}
			if (min > max) {
				return { ok: false, message: t('task_proc_util', 'Minimum workers cannot exceed maximum workers') }
			}
			return { ok: true, min, max, poll }
		},

		async save() {
			const result = this.validate()
			if (!result.ok) {
				this.error = result.message
				return
			}
			try {
				const { data } = await axios.put(
					generateOcsUrl('task_proc_util/config'),
					{
						minWorkers: result.min,
						maxWorkers: result.max,
						pollInterval: result.poll,
						enabled: this.config.enabled,
					},
				)
				this.config = data.ocs.data
				this.minWorkersStr = String(this.config.min_workers)
				this.maxWorkersStr = String(this.config.max_workers)
				this.pollIntervalStr = String(this.config.poll_interval)
				showSuccess(t('task_proc_util', 'Settings saved'))
			} catch (e) {
				this.error = e.response?.data?.ocs?.data?.message
					|| e.response?.data?.message
					|| t('task_proc_util', 'Failed to save settings')
				showError(this.error)
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
}

.tpu-field {
	max-width: 320px;
}

.tpu-error {
	color: var(--color-error);
}
</style>
