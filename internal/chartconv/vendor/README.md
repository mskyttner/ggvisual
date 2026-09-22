Vendored JS for the `html-page-offline` format's self-contained export (embedded via
`//go:embed`, no CDN dependency at render or view time).

Pinned to the exact patch versions `@6`/`@6`/`@7` resolved to on jsdelivr as of 2026-09-22
(matching the major versions `html`/`html-page`/`html-body` already reference via CDN):

| file               | package      | version |
|--------------------|--------------|---------|
| `vega.min.js`       | vega         | 6.4.0   |
| `vega-lite.min.js`  | vega-lite    | 6.4.3   |
| `vega-embed.min.js` | vega-embed   | 7.2.0   |

Fetched via:

```
curl -sL -o vega.min.js       https://cdn.jsdelivr.net/npm/vega@6.4.0
curl -sL -o vega-lite.min.js  https://cdn.jsdelivr.net/npm/vega-lite@6.4.3
curl -sL -o vega-embed.min.js https://cdn.jsdelivr.net/npm/vega-embed@7.2.0
```

To refresh: re-resolve the current version for each major (`curl -s
"https://data.jsdelivr.com/v1/packages/npm/<pkg>/resolved?specifier=<major>"`), re-run the
`curl` commands above with the new versions, and update the table.
