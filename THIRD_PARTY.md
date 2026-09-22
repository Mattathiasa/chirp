# Third-party assets

Licences for everything bundled into the Chirp binary or the static site.

## Fonts

All three families are licensed under the SIL Open Font License 1.1. The woff2
files live in `internal/web/static/fonts/` and are embedded into the binary
with `go:embed`, so the UI renders correctly with no network access and no
external host in the CSP (`font-src 'self'`).

| Font | Upstream | Licence text | Weights |
| --- | --- | --- | --- |
| Bricolage Grotesque | [ateliertriay/bricolage](https://github.com/ateliertriay/bricolage) | [`BricolageGrotesque-OFL.txt`](internal/web/static/fonts/BricolageGrotesque-OFL.txt) | 400, 500, 600, 700, 800 |
| Hanken Grotesk | [marcologous/hanken-grotesk](https://github.com/marcologous/hanken-grotesk) | [`HankenGrotesk-OFL.txt`](internal/web/static/fonts/HankenGrotesk-OFL.txt) | 400, 500, 600, 700, 800 |
| JetBrains Mono | [JetBrains/JetBrainsMono](https://github.com/JetBrains/JetBrainsMono) | [`JetBrainsMono-OFL.txt`](internal/web/static/fonts/JetBrainsMono-OFL.txt) | 400, 500, 600, 700, 800 |

The OFL requires that the licence and copyright notice accompany the fonts.
Each family's unmodified `OFL.txt`, carrying its own copyright line, sits next
to its woff2 files and is embedded alongside them. None of the fonts have been
modified or renamed beyond subsetting to woff2, so no Reserved Font Name
restriction applies.

## Go dependencies

Pinned in `go.mod` / `go.sum`:

| Module | Licence | |
| --- | --- | --- |
| `github.com/flynn/noise` | BSD-3-Clause | direct |
| `github.com/grandcat/zeroconf` | MIT | direct |
| `go.etcd.io/bbolt` | MIT | direct |
| `golang.org/x/crypto` | BSD-3-Clause | direct |
| `github.com/cenkalti/backoff` | MIT | indirect |
| `github.com/miekg/dns` | BSD-3-Clause | indirect |
| `golang.org/x/net` | BSD-3-Clause | indirect |
| `golang.org/x/sys` | BSD-3-Clause | indirect |

Licences read from each module's own `LICENSE` file in the module cache, not
from memory. All are permissive and compatible with this project's MIT licence.

Run `make vuln` for the current vulnerability report.

## Static UI assets

- The SVG favicon in `internal/web/static/index.html` is original work.
- The icons in `app.js` are hand-written inline SVG paths, covered by this
  project's own licence.
