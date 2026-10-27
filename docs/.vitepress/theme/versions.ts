import { ref } from 'vue'

// A version of the documentation, as /versions.json at the root of the
// site lists them: the releases, newest first, then dev.
export interface Version {
  version: string
  title: string
  path: string
}

export const versions = ref<Version[]>([])

let loaded = false

// load reads /versions.json once, when a page opens, so an old version
// learns of newer ones without being built again.
export function load() {
  if (loaded) return
  loaded = true
  fetch('/versions.json')
    .then((r) => (r.ok ? r.json() : []))
    .then((v: Version[]) => (versions.value = v))
    .catch(() => {})
}

// current is the version that this build is, by its base.
export function current(): string {
  return import.meta.env.BASE_URL
}
