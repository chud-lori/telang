# Telang docs site

This directory is the source for the Telang GitHub Pages site.

- `index.md` → `/` (home)
- `setup.md` → `/setup/` (setup & usage)
- `_includes/nav.html` → shared top nav
- `_config.yml` → Jekyll site config (cayman theme, `baseurl: /telang`)

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
