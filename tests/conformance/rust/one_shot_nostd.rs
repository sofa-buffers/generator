// corelib-rs-no-std's twin of one_shot_std.rs: try_decode returns a new value,
// so the shared drivers' `one_shot!(T, wire)` is that call unchanged.
#[allow(unused_macros)]
macro_rules! one_shot {
    ($t:ty, $wire:expr) => {
        <$t>::try_decode($wire)
    };
}
