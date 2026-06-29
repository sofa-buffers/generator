# Real-world example — connected-vehicle fleet telemetry

A small but realistic schema split across **three files** to show how a real
project organizes shared types with cross-file `$ref`:

```
realworld/
├── common.yaml             # $defs only: GeoPoint, Vector3, Timestamp
├── diagnostics.yaml        # $defs only: Gear, FaultFlags, DiagnosticCode
│                           #   (DiagnosticCode itself $refs common.yaml)
└── vehicle_telemetry.yaml  # the message: VehicleTelemetry, $refs both files
```

Generate it for any target:

```sh
sofabgen --lang go --in examples/messages/realworld/vehicle_telemetry.yaml --out out/telemetry
# ...or cpp, rust, python, typescript, csharp, java, c
```

## What it demonstrates

- **Cross-file `$ref` across multiple files** — `vehicle_telemetry.yaml` pulls
  `Timestamp`, `GeoPoint`, `Vector3` from `common.yaml` and `Gear`,
  `FaultFlags`, `DiagnosticCode` from `diagnostics.yaml`.
- **A definition referenced multiple times → one shared type** — `Vector3` is
  used by both `velocity` and `acceleration` and is emitted once.
- **Transitive cross-file refs** — `DiagnosticCode` (in `diagnostics.yaml`)
  itself `$ref`s `GeoPoint` and `Timestamp` from `common.yaml`; using
  `DiagnosticCode` from the message flattens the whole chain automatically.
- **A library file with no messages** — `common.yaml`/`diagnostics.yaml` contain
  only `$defs`; they are never generated on their own, only merged where used.
- **Realistic field design** — nested structs, an enum with a `default`, a
  `[Flags]`-style bitfield, a fixed array (`tire_kpa[4]`), `unit` annotations,
  a `default` (`battery_pct = 100`), and a `deprecated` field that older readers
  can still skip. Field ids are stable so the frame stays forward/backward
  compatible.

The schema validates, generates for all eight languages, and round-trips a real
telemetry frame against the corelib (exercised by the test suite).
