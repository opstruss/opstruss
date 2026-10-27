import { defineConfig } from 'vitepress'

// Each build is the documentation of one version: /v<major>.<minor>/ for a
// release and /dev/ for main, which DOCS_BASE names.
export default defineConfig({
  title: 'OpsTruss',
  description: 'Event-driven workflow orchestration that runs any tool and hands its typed results to the next.',
  base: process.env.DOCS_BASE ?? '/dev/',
  cleanUrls: true,
  srcExclude: ['examples/**'],
  themeConfig: {
    nav: [
      { text: 'Getting started', link: '/getting-started/' },
      { text: 'Guides', link: '/guides/structure-a-project' },
      { text: 'Reference', link: '/reference/opstruss-yaml' },
      { text: 'Plugins', link: '/plugins' },
    ],
    sidebar: [
      {
        text: 'Getting started',
        items: [
          { text: 'Install', link: '/getting-started/install' },
          { text: 'Your first project', link: '/getting-started/' },
        ],
      },
      {
        text: 'Guides',
        items: [
          { text: 'Structure a project', link: '/guides/structure-a-project' },
          { text: 'Secrets with sops', link: '/guides/secrets' },
          { text: 'Triggers and the daemon', link: '/guides/triggers' },
          { text: 'CI with GitHub Actions', link: '/guides/ci' },
          { text: 'Writing a plugin', link: '/guides/writing-a-plugin' },
        ],
      },
      {
        text: 'Reference',
        items: [
          { text: 'opstruss.yaml', link: '/reference/opstruss-yaml' },
          { text: 'opstruss.plugin.yaml', link: '/reference/plugin-yaml' },
          { text: 'CLI', link: '/reference/cli/' },
          { text: 'Expressions', link: '/reference/expressions' },
          { text: 'Built-in tasks', link: '/reference/builtin-tasks' },
          { text: 'Program contract', link: '/reference/program-contract' },
          { text: 'Exit codes and JSON', link: '/reference/exit-codes-and-json' },
        ],
      },
      { text: 'Plugins', link: '/plugins' },
      {
        text: 'Concepts',
        items: [{ text: 'How OpsTruss works', link: '/concepts/how-opstruss-works' }],
      },
    ],
    search: { provider: 'local' },
    socialLinks: [{ icon: 'github', link: 'https://github.com/opstruss/opstruss' }],
  },
})
