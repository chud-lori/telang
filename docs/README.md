# Telang docs site

This directory is the source for the Telang GitHub Pages site.

| File | Serves | Built by |
|---|---|---|
| `index.html` | `/` (home and reference) | nobody: Jekyll copies it verbatim |
| `setup.md` | `/setup/` (setup and usage) | `_layouts/page.html` |
| `_layouts/`, `_includes/`, `assets/` | the setup page's chrome and styles | Jekyll |
| `_config.yml` | site config, `baseurl: /telang`, no theme gem | Jekyll |

`index.html` is a single self-contained file with its own inline CSS and JS,
so it has no front matter and Jekyll passes it straight through. Its design
direction is `DESIGN.md` at the repo root. Edit the page itself, not a
layout.

Anything user-facing that changes on that page has to change in the root
`README.md` too, and the other way round.

## Enable the site

In the GitHub repo, go to **Settings → Pages**, then:

| Field | Value |
|---|---|
| Source | Deploy from a branch |
| Branch | `main` |
| Folder | `/docs` |

After ~30 s, the site is live at
`https://chud-lori.github.io/telang/`.

If you fork this under a different repo name, update `baseurl` in
`_config.yml` to match (it must equal the repo name with a leading `/`,
or be empty for a user/org page).

## Local preview

```bash
gem install bundler jekyll
cd docs
bundle init && bundle add jekyll github-pages
bundle exec jekyll serve
```

Then open <http://localhost:4000/telang/>.
