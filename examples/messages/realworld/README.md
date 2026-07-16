# Real-world example — connected-vehicle fleet telemetry

A small but realistic schema split across **three files** to show how a real
project organizes shared types with cross-file `$ref`:

```
realworld/
├── common.yaml             # $defs only: GeoPoint, Vector3, Timestamp
├── diagnostics.yaml        # $defs only: Gear, FaultFlags, Features,
│                           #   SensorSample, DiagnosticCode
│                           #   (DiagnosticCode itself $refs common.yaml)
└── vehicle_telemetry.yaml  # the message: VehicleTelemetry, $refs both files
```

Generate it for any target:

```sh
sofabgen --lang go --in examples/messages/realworld/vehicle_telemetry.yaml --out out/telemetry
# ...or cpp, rust, zig, python, typescript, csharp, java, c
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
- **The complete type/feature surface** — while staying a plausible telemetry
  frame, the schema now exercises everything the generator supports so it doubles
  as a cross-file conformance schema:
  - **Every scalar** — `u8..u64`, `i8..i64` (`cabin_temp_c`, `motor_torque_nm`,
    `net_power_w`, `trip_energy_wh`), `fp32`/`fp64`, and `boolean` (`charging`).
  - **`string` and `blob`** with `maxlen`, including a `blob` default.
  - **Both enum member forms** (`{value, description}` and the bare shorthand in
    `Gear`) and a **bitfield with per-flag `default`s** (`Features`).
  - **`union`** both standalone (`aux_sensor` → `SensorSample`, with `default_id`)
    and as an array element (`energy_sources`).
  - **Arrays of every element kind** — scalar (`tire_kpa`, with an array
    `default`), string (`warnings`), blob (`ecu_sigs`), struct (`recent_codes`),
    enum (`gear_history`), boolean (`doors_open`), bitfield (`wheel_faults`),
    union (`energy_sources`) and array-of-array (`cell_mv`, a matrix).
  - **All per-field metadata** — `unit`, `description`, `decimals` (a precision
    hint on `GeoPoint` and `battery_temp_c`), `deprecated`, `maxlen`, `count`,
    and `default`s of every flavour (scalar, string, blob, float, `i64`-as-string
    and array).

  Every array is bounded (`count`) and every `string`/`blob` has a `maxlen`, so
  the schema also generates for the heap-less targets (C and the `no_std`
  profiles), not only the dynamic-allocation ones.

The schema validates, generates for all nine languages, and round-trips a real
telemetry frame against the corelib (exercised by the test suite).
