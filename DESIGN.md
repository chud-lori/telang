# DESIGN.md

Design direction for the Telang docs site (`docs/`).

Nothing here is invented. Every value below is extracted from something the
repository already decided: the brand icon, the stylesheet that shipped in
commit `1117553` ("rebuild Pages site with custom dark theme"), or the
product's own README voice. Each entry cites its source. Items with no
source in the repo are listed under **Open** as questions for the owner,
not filled in with a guess.

## Identity

Telang is a single Go binary that turns a private Telegram channel into the
disk behind an S3 endpoint. It is a self-hosted tool for one operator, not a
service with customers.

- Name and product definition: `README.md:12-18`.
- The site must read as operator documentation, not as marketing. Source:
  the README leads with the limitation ("Not for production, customer data,
  or high-throughput public asset serving") before any feature,
  `README.md:20-21`.
- Two audiences, one page: someone deciding whether to run it, and someone
  already running it who needs the flag or config key. Source: the repo
  ships `setup.md`, `COMPAT.md`, `BENCHMARKS.md`, and `ARCHITECTURE.md` as
  separate operator documents, so the landing page is the index over them.

## Personality

Blunt, technical, risk-forward. The product says what breaks before it says
what works.

- "Telegram can ban the account; your data can vanish. That is the deal."
  `README.md:22`.
- Section heading already in the site: "Be honest with yourself",
  `docs/_layouts/home.html:201`.
- Lowercase nav labels ("home", "setup") are an existing voice choice,
  `docs/_includes/header.html:10-11`. Kept.
- No exclamation, no superlatives, no growth language anywhere in the
  README. Kept.

## Palette

Two sources agree, so the palette is treated as settled.

Source A, the brand icon `docs/assets/brand/telang-icon.png`: dark navy
ground, a Telegram-blue paper plane, a white bucket, an amber padlock.

Source B, the shipped stylesheet `docs/assets/css/style.css`:

| Token | Dark value | Source |
|---|---|---|
| Background | `#0b0f1a` | `style.css:5` |
| Raised surface | `#131826` | `style.css:6` |
| Body text | `#e6e9ef` | `style.css:8` |
| Muted text | `#8b95a5` | `style.css:9` |
| Primary (blue) | `#5eb3ff` | `style.css:13` |
| Accent (amber) | `#ff9f5b` | `style.css:15` |
| Warning | `#facc15` | `style.css:17` |

Light values come from the same file's `prefers-color-scheme: light` block,
`style.css:29-46`: background `#fafbfc`, surface `#ffffff`, text `#1a2030`,
muted `#5a6478`, primary `#0070d4`, accent `#d77035`.

Two corrections were forced by WCAG AA and are the only deviations from the
shipped values:

- `--text-dim: #5a6478` on the dark background (`style.css:10`) measures
  3.22:1, below the 4.5:1 floor for normal text. Not reused.
- The light-mode accent `#d77035` on `#fafbfc` measures 3.24:1. It is kept
  for rules and markers, and a darkened `#a34e12` (5.54:1) is used wherever
  the accent carries text.

Colour carries meaning rather than decorating: blue marks the wire side
(Telegram transport, S3 protocol), amber marks the crypto and risk side.
That split is taken from the icon, where the plane is blue and the padlock
is amber.

## Typography

- System sans stack, `style.css:53`. Reason on the record: the site ships no
  webfont and no build step, so type falls to the platform UI font.
- System mono stack, `style.css:77`, used for commands, config keys, flags,
  and file names only. Not used for headings: the product is a CLI, so
  monospace has to keep meaning "this is literal text you type".

## Mood

A reference manual you can read at 2am while your daemon is down. Calm
ground, high-contrast text, one accent used sparingly at the moments that
matter (encryption, data loss).

Dark is the default rather than a trend: the audience operates a terminal
daemon, and the shipped stylesheet already declares `color-scheme: dark
light` with dark first (`style.css:3-5`). A light theme is complete and
reachable by an explicit toggle.

## Dial

`Dial: ENERGY 2 / RHYTHM 2 / MOTION 1`

Derived, not confirmed. Awaiting the owner's confirmation.

- ENERGY 2: the palette and the icon have a real identity (navy, blue,
  amber) but the README voice never raises its voice.
- RHYTHM 2: the content is genuinely different shape to shape (a decision
  section, reference tables, a byte-layout diagram, mode-specific setup), so
  sections vary; a documentation spine keeps them consistent.
- MOTION 1: an operator reference has no reason to animate. Hover and focus
  states, and instant state changes on toggles, only.

## Constraints

- GitHub Pages, built from `docs/` on `main`. `docs/README.md:10-19`.
- `baseurl: /telang`, so in-page links stay relative. `docs/_config.yml:7`.
- No build step and no runtime dependency: the existing JS layer is plain
  ES5-era script with the comment "No build step, no dependencies"
  (`docs/assets/js/main.js:1`). The landing page keeps that promise by
  inlining its own CSS and JS.
- Every fact on the page must be traceable to source, to a shipped document
  (`BENCHMARKS.md`, `COMPAT.md`), or be omitted.
- No statistics without a cited run. The only numbers the repo can support
  are the benchmark table in `BENCHMARKS.md:19-26`, which is explicitly
  daemon overhead against a fake Telegram, not throughput.

## Open

Questions for the owner. These are not filled in with invented answers.

1. **The `go install` path is broken and neither spelling works.** `go.mod:1`
   declares `module github.com/telang/telang`, but the repository is at
   `github.com/chud-lori/telang`. `go install github.com/telang/telang/...`
   points at a module that is not this repo, and
   `go install github.com/chud-lori/telang/...` fails the module path check.
   The page now documents the clone-and-build path, which does work. Which
   should change, the module path or the repo?
2. **No released version exists.** `git tag` is empty and there is no
   release workflow, no goreleaser config, and no Makefile. The page
   therefore states no version and offers no binary download. Is a tagged
   release planned, and should the page carry a version badge then?
3. **No published Docker image.** The repo has a `Dockerfile` but no
   registry reference. The page documents `docker build` from a clone. Is an
   image going to be pushed somewhere?
4. **Icon provenance and licence are not recorded** anywhere in the repo.
   The page reuses `docs/assets/brand/telang-icon.png` as-is and creates no
   new brand asset.
5. **Dial confirmation.** ENERGY 2 / RHYTHM 2 / MOTION 1 is derived from the
   README voice and the shipped stylesheet, not stated by the owner.
6. **No typeface decision is recorded**, only a system stack fallback. If a
   webfont is ever wanted, that is a direction call, not a filter call.
