# Fletcher docs site

The public documentation site for Fletcher, built with
[VitePress](https://vitepress.dev).

Only **public, end-user** material lives here. Internal docs (roadmap, testing,
research, design) stay in the repo root and `docs/` and are deliberately not part
of this site.

## Develop

Uses [pnpm](https://pnpm.io). Node 20.19+ (see `.nvmrc` for the pinned version).

```sh
pnpm install
pnpm dev        # local dev server with hot reload
pnpm build      # production build to .vitepress/dist
pnpm preview    # serve the production build locally
```

## Layout

```
docs/site/
  .vitepress/
    config.ts            site config, nav, sidebar
    theme/               custom theme (clean, dark-first)
  guide/                 getting started, guides, operations
  advanced/              deep dives
  index.md               home page
```
