import DefaultTheme from 'vitepress/theme'
import { h } from 'vue'
import Versions from './Versions.vue'
import Banner from './Banner.vue'

// The default theme, with the versions of the documentation in the nav
// bar and a banner on any version but the latest.
export default {
  extends: DefaultTheme,
  Layout: () => h(DefaultTheme.Layout, null, {
    'nav-bar-content-after': () => h(Versions),
    'layout-top': () => h(Banner),
  }),
}
