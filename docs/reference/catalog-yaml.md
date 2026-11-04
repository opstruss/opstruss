---
title: opstruss.catalog.yaml
---

<!-- Written by go run ./internal/docgen; edit the source, not this page. -->

::: v-pre

# opstruss.catalog.yaml

The root of an OpsTruss catalog: a directory of plugins and components.

Keys that start with x- are left for your own use.

| Key | Type | Description |
|---|---|---|
| `opstruss` | string | The oldest OpsTruss version that reads this file, as MAJOR.MINOR. |
| `shared` | list of string | Paths that every package of the catalog may use, relative to its root. |
| `packages` | list of string or [packages](#packages) | The packages of the catalog, which a project uses as &lt;catalog&gt;.&lt;package&gt; or alone as &lt;repository&gt;/&lt;package&gt;@&lt;ref&gt;, by name, wherever its directory is: a directory relative to the root, named by its last directory; a glob such as tools/\*, whose \* matches within one level and takes the directories with opstruss.plugin.yaml or opstruss.yaml; or a name with a path. Without it, every subdirectory of the root with opstruss.plugin.yaml or opstruss.yaml is a package, named by its directory. |

## packages {#packages}

A package with a name of its own.

| Key | Type | Description |
|---|---|---|
| `name` (required) | string | The name of the package. |
| `path` (required) | string | The directory of the package, relative to the root, without a glob. |

:::
