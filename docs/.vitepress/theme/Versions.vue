<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute } from 'vitepress'
import { versions, load, current } from './versions'

const route = useRoute()
onMounted(load)
const here = computed(() => versions.value.find((v) => v.path === current()))
// The same page in another version, which may not have it.
const page = computed(() => route.path.slice(current().length))
function go(e: Event) {
  window.location.href = (e.target as HTMLSelectElement).value + page.value
}
</script>

<template>
  <div v-if="versions.length" class="versions">
    <select :value="here?.path" @change="go">
      <option v-for="v in versions" :key="v.path" :value="v.path">{{ v.title }}</option>
    </select>
  </div>
</template>

<style scoped>
.versions { display: flex; align-items: center; padding-left: 16px; }
select { background: var(--vp-c-bg-soft); border: 1px solid var(--vp-c-divider); border-radius: 6px; padding: 2px 6px; font-size: 13px; }
</style>
