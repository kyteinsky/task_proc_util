/**
 * SPDX-FileCopyrightText: 2026 Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */
import { createAppConfig } from '@nextcloud/vite-config'

const isProduction = process.env.NODE_ENV === 'production'

export default createAppConfig({
	adminSettings: 'src/adminSettings.js',
}, {
	config: {
		build: {
			cssCodeSplit: true,
		},
	},
	inlineCSS: { relativeCSSInjection: true },
	minify: isProduction,
})
