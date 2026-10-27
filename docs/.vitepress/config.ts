import { defineConfig } from 'vitepress'

// Each build is the documentation of one version: /v<major>.<minor>/ for a
// release and /dev/ for main, which DOCS_BASE names.
export default defineConfig({
  title: 'OpsTruss',
  description: 'Event-driven workflow orchestration that runs any tool and hands its typed results to the next.',
  base: process.env.DOCS_BASE ?? '/dev/',
  cleanUrls: true,
  srcExclude: ['examples/**'],
  markdown: {
    config(md) {
      // ${{ }} in inline code is an expression of OpsTruss, not of Vue, as
      // it already is in code blocks.
      const code = md.renderer.rules.code_inline!
      md.renderer.rules.code_inline = (tokens, idx, options, env, self) =>
        code(tokens, idx, options, env, self).replace(/^<code/, '<code v-pre')
    },
  },
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
          { text: 'Tasks and steps', link: '/guides/tasks' },
          { text: 'Services', link: '/guides/services' },
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
          { text: 'Compatibility', link: '/reference/compatibility' },
        ],
      },
      { text: 'Plugins', link: '/plugins' },
      {
        text: 'Concepts',
        items: [{ text: 'How OpsTruss works', link: '/concepts/how-opstruss-works' }],
      },
      { text: 'Troubleshooting', link: '/troubleshooting' },
    ],
    search: { provider: 'local' },
    socialLinks: [{ icon: 'github', link: 'https://github.com/opstruss/opstruss' }],
  },
})
