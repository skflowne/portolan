# Tier A fixture contract

`expected.json` and the files under `expected/` are the independently authored contract for this
pinned source fixture and `tsgo` version. Positions are zero-based UTF-16 code-unit offsets and
ranges are half-open. Paths are fixture-relative so the gate can compare exact identities in any
checkout. Update these files only after reviewing a deliberate fixture or pinned-analyzer change;
never generate them through Portolan's LSP, tool, normalization, rendering, capping, or telemetry
code.

`expected.json` covers the remaining structured MCP output from `find_references` plus the complete
telemetry event sequence. Tier A compares each raw structured output to this data before decoding it
into Go types, so missing required zero-valued fields and unexpected keys are contract failures.

`expected/outline_*.txt` and `expected/definition_*.txt` pin the two compact-text tools byte for byte
across found, truncated, honest-empty, and soft-error responses. The definition fixtures cover exact
single and parameter-property declarations, provider-ordered overloads, Markdown-fence safety, and
cap-before-enrichment. Because comparison is exact string equality, these files also pin source
bytes, indentation, line endings, blank-line placement, order, and footers.
They record the pinned analyzer's public `DocumentSymbol` conventions as reviewed against the
source: imported bindings are symbols, parameter properties are separate children, and declaration
ranges may span several lines. Selection ranges, generation, and staleness are deliberately absent
because they are not agent-facing; they stay authoritative in typed results and telemetry.

`src/definitions.ts` and `src/definition_use.ts` own the overload and embedded-backtick definition
cases. `src/empty.ts` exists so the outline honest-empty response has a real-daemon home.
