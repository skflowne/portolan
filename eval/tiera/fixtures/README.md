# Tier A fixture contract

`expected.json` is the independently authored contract for this pinned source fixture and
`tsgo` version. Positions are zero-based UTF-16 code-unit offsets and ranges are half-open.
Paths are fixture-relative so the gate can compare exact identities in any checkout.

The expected outline records the pinned analyzer's public `DocumentSymbol` conventions as
reviewed against the source: imported bindings are symbols, parameter properties are separate
children, and synthetic constructor/callback selection ranges may be zero-width. Update this
file only after reviewing a deliberate fixture or pinned-analyzer change; never generate it
through Portolan's LSP, tool, normalization, capping, or telemetry code.
