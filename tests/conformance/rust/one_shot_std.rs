// A whole-buffer decode into a FRESH destination, handed back as the old
// `try_decode(data) -> Result<T, DecodeError>` was. corelib-rs's try_decode
// writes into a destination the caller owns (generator#755) and
// corelib-rs-no-std's returns a new value, so the drivers shared by both
// profiles spell the call `one_shot!(T, wire)` and run.sh puts this file -- or
// one_shot_nostd.rs -- in front of them. A decode into a REUSED destination is
// checked by tests/conformance/lib/check_reused_destination.py and
// reuse_alloc.rs, not here.
#[allow(unused_macros)]
macro_rules! one_shot {
    ($t:ty, $wire:expr) => {{
        let mut _out = <$t>::default();
        <$t>::try_decode($wire, &mut _out).map(|()| _out)
    }};
}
