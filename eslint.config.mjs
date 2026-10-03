/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

import { recommended, recommendedJavascript } from '@nextcloud/eslint-config'

export default [
	...recommended,
	...recommendedJavascript,

	{
		name: 'task_proc_util/rule-overrides',
		rules: {
			'no-console': ['warn', { allow: ['debug', 'warn', 'error'] }],
			'vue/v-on-event-hyphenation': 'off',
			'vue/attribute-hyphenation': 'off',
		},
		files: ['src/**/*.vue', 'src/**/*.js'],
	},
]
