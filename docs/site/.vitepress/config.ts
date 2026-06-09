import { defineConfig } from 'vitepress'

// https://vitepress.dev/reference/site-config
export default defineConfig({
  title: 'Fletcher',
  description: 'Private agent compute on hardware you own.',
  lang: 'en-US',
  cleanUrls: true,
  lastUpdated: true,

  // Default to dark; the toggle is still available.
  appearance: 'dark',

  head: [
    ['meta', { name: 'theme-color', content: '#0c0c0e' }],
    ['meta', { property: 'og:type', content: 'website' }],
    ['meta', { property: 'og:title', content: 'Fletcher' }],
    [
      'meta',
      {
        property: 'og:description',
        content: 'Private agent compute on hardware you own.',
      },
    ],
  ],

  themeConfig: {
    // https://vitepress.dev/reference/default-theme-config
    nav: [
      { text: 'Guide', link: '/guide/introduction', activeMatch: '/guide/' },
      { text: 'Advanced', link: '/advanced/networking', activeMatch: '/advanced/' },
      {
        text: 'GitHub',
        link: 'https://github.com/joshjon/fletcher',
      },
    ],

    sidebar: {
      '/guide/': sidebarGuide(),
      '/advanced/': sidebarGuide(),
    },

    socialLinks: [
      { icon: 'github', link: 'https://github.com/joshjon/fletcher' },
    ],

    search: {
      provider: 'local',
    },

    outline: { level: [2, 3], label: 'On this page' },

    editLink: {
      pattern:
        'https://github.com/joshjon/fletcher/edit/main/docs/site/:path',
      text: 'Edit this page on GitHub',
    },

    docFooter: {
      prev: 'Previous',
      next: 'Next',
    },

    footer: {
      message: 'Released under the Apache-2.0 License.',
      copyright: 'Copyright 2026 Joshua Jon',
    },
  },
})

function sidebarGuide() {
  return [
    {
      text: 'Getting started',
      collapsed: false,
      items: [
        { text: 'Introduction', link: '/guide/introduction' },
        { text: 'Installation', link: '/guide/installation' },
        { text: 'Networking & first run', link: '/guide/networking' },
        { text: 'Pair a device', link: '/guide/pairing' },
        { text: 'Your first agent', link: '/guide/first-agent' },
      ],
    },
    {
      text: 'Guides',
      collapsed: false,
      items: [
        { text: 'Jobs & cron', link: '/guide/jobs' },
        { text: 'Durable sessions', link: '/guide/sessions' },
        { text: 'Publishing ports', link: '/guide/publishing' },
        { text: 'Deploying apps', link: '/guide/deploy' },
        { text: 'Remote control', link: '/guide/remote' },
      ],
    },
    {
      text: 'Operations',
      collapsed: false,
      items: [
        { text: 'Configuration', link: '/guide/configuration' },
        { text: 'Managing the daemon', link: '/guide/daemon' },
        { text: 'Security', link: '/guide/security' },
        { text: 'Troubleshooting', link: '/guide/troubleshooting' },
      ],
    },
    {
      text: 'Advanced',
      collapsed: false,
      items: [
        { text: 'Networking deep dive', link: '/advanced/networking' },
        { text: 'Runtimes & base images', link: '/advanced/runtimes' },
        { text: 'Public web over HTTPS', link: '/advanced/public-web' },
        { text: 'Building from source', link: '/advanced/building' },
      ],
    },
  ]
}
