# Tier A fixture contract

`expected.json` and the files under `expected/` are the independently authored contract for this
pinned source fixture and `tsgo` version. Positions are zero-based UTF-16 code-unit offsets and
ranges are half-open. Paths are fixture-relative so the gate can compare exact identities in any
checkout. Update these files only after reviewing a deliberate fixture or pinned-analyzer change;
never generate them through Portolan's LSP, tool, normalization, rendering, capping, or telemetry
code.

`expected.json` covers `find_definition`, the tool that still answers with structured MCP output,
plus the telemetry event sequence. Tier A compares each raw structured definition output to this
data before decoding it into Go types, so missing required zero-valued fields and unexpected keys
are contract failures.

`expected/references_circle.txt` pins `find_references`' complete agent-facing text byte for byte,
including canonical source and result paths, grouped range identity and order, and the reference/file
footer. `expected/outline_*.txt` similarly pin `get_outline` across found, truncated, honest-empty,
and soft-error responses. Because the comparisons use exact string equality, these files also pin
blank-line placement and completion state. The outline files record the pinned analyzer's public
`DocumentSymbol` conventions as reviewed against the source: imported bindings are symbols,
parameter properties are separate children, and declaration ranges may span several lines.
Selection ranges, generation, and staleness are deliberately absent
because they are not agent-facing; they stay in typed internal results and owner-level tests, while
Tier A continues to assert the references telemetry event.

`src/empty.ts` exists so the honest-empty response has a real-daemon home.
