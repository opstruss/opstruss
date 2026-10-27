<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute } from 'vitepress'
import { versions, load, current } from './versions'

const route = useRoute()
onMounted(load)
const latest = computed(() => versions.value.find((v) => v.version !== 'dev'))
const here = computed(() => versions.value.find((v) => v.path === current()))
const show = computed(() => latest.value && here.value && here.value.path !== latest.value.path)
const link = computed(() => latest.value!.path + route.path.slice(current().length))
</script>

<template>
  <div v-if="show" class="banner">
    This is the documentation of {{ here!.version }}; the latest is
    <a :href="link">{{ latest!.version }}</a>.
  </div>
</template>

<style scoped>
.banner { padding: 8px 16px; text-align: center; font-size: 14px; background: var(--vp-c-warning-soft); }
</style>
