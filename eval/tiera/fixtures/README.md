# Tier A fixture contract

`expected.json` and the files under `expected/` are the independently authored contract for this
pinned source fixture and `tsgo` version. Positions are zero-based UTF-16 code-unit offsets and
ranges are half-open. Paths are fixture-relative so the gate can compare exact identities in any
checkout. Update these files only after reviewing a deliberate fixture or pinned-analyzer change;
never generate them through Portolan's LSP, tool, normalization, rendering, capping, or telemetry
code.

`expected.json` covers the tools that still answer with structured MCP output — `find_definition`
and `find_references` — plus the telemetry event sequence. Tier A compares each raw structured
output to this data before decoding it into Go types, so missing required zero-valued fields and
unexpected keys are contract failures.

`expected/outline_*.txt` pin `get_outline`'s complete agent-facing text byte for byte across its
found, truncated, honest-empty, and soft-error responses. Because the comparison is exact string
equality, these files also pin indentation, blank-line placement, symbol order, and the footer.
They record the pinned analyzer's public `DocumentSymbol` conventions as reviewed against the
source: imported bindings are symbols, parameter properties are separate children, and declaration
ranges may span several lines. Selection ranges, generation, and staleness are deliberately absent
because they are not agent-facing; they stay in the typed result, which `find_definition` and
`find_references` exercise instead.

`src/empty.ts` exists so the honest-empty response has a real-daemon home.
