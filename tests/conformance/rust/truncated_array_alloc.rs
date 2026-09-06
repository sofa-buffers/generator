// A TRUNCATED bounded array must not cost its whole declared count — measured,
// not asserted.
//
// generator#505 made a schema-bounded native array reserve the wire count its own
// `count > N` reject has just approved, which is ARCHITECTURE §9.5 shape A: one
// allocation, in the exact size, instead of doubling into the container. The half
// that is easy to miss is that `count > N` proves only that the wire stayed inside
// the SCHEMA's N, and `schema/sofabuffers-schema-v1.json` caps N at 2147483647. A
// reserve taken on N alone therefore hands a truncated packet the field's declared
// worst case: for `arr: array<u64>, count: 2000000`, a FOUR-byte prefix — the
// array header and its count word, and not one element byte — buys 16,000,000
// bytes and then returns `Incomplete`. Rust's allocation-failure path is
// `handle_alloc_error`, a process abort and not a catchable error, so this is not
// a cost to leave to whoever writes the schema.
//
// The generated arm therefore clamps: `reserve_exact(count.min(<ceiling>))`, the
// ceiling being the resolved `max_dyn_array_count` — the number this project
// already treats as its amplification barrier (generator#102). The clamp is a
// CAPACITY decision with no semantic effect, which is the other half of this row:
// a well-formed message that really carries more than the ceiling must still
// decode in full, growing past the hint exactly as it did before.
//
// This is a MEASUREMENT and not a `try_decode(..).is_err()` row for the same
// reason skipped_blob_alloc.rs is: the over-allocating build returns `Incomplete`
// too. Only counting the bytes tells the two apart.
//SOFAB_IMPORT

use std::alloc::{GlobalAlloc, Layout, System};
use std::sync::atomic::{AtomicUsize, Ordering};

static ALLOCATED: AtomicUsize = AtomicUsize::new(0);

/// The system allocator with a running total of every byte handed out. Only
/// growth is counted, for the reason skipped_blob_alloc.rs gives.
struct Counting;

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        ALLOCATED.fetch_add(l.size(), Ordering::Relaxed);
        System.alloc(l)
    }
    unsafe fn dealloc(&self, p: *mut u8, l: Layout) {
        System.dealloc(p, l)
    }
    unsafe fn realloc(&self, p: *mut u8, l: Layout, new: usize) -> *mut u8 {
        if new > l.size() {
            ALLOCATED.fetch_add(new - l.size(), Ordering::Relaxed);
        }
        System.realloc(p, l, new)
    }
}

#[global_allocator]
static A: Counting = Counting;

fn put_varint(out: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        out.push((v as u8 & 0x7f) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

fn main() {
    // The schema's own bound, and the ceiling the crate was generated with.
    const DECLARED: u64 = 2_000_000;
    const CEILING: usize = 65_536;
    const ELEM: usize = 8; // u64

    // id 0, T_VARINTARRAY_UNSIGNED (wire type 3), announcing DECLARED elements —
    // and then the message ends.
    let mut msg = Vec::new();
    put_varint(&mut msg, (0u64 << 3) | 3);
    put_varint(&mut msg, DECLARED);
    assert!(msg.len() <= 8, "the whole attack is a header: {} bytes", msg.len());

    match Big::try_decode(&msg) {
        Err(DecodeError::Incomplete) => {}
        other => panic!("a truncated array must be Incomplete, got {other:?}"),
    }

    let reps = 8usize;
    let before = ALLOCATED.load(Ordering::Relaxed);
    for _ in 0..reps {
        let r = Big::try_decode(&msg);
        std::hint::black_box(&r);
    }
    let per = (ALLOCATED.load(Ordering::Relaxed) - before) / reps;
    println!(
        "    a {}-byte truncated array header allocates {per} bytes per decode",
        msg.len()
    );

    // Generous by an order of magnitude on purpose: the row is here to catch the
    // DECLARED count reaching the allocator (16,000,000 bytes), not to pin the
    // exact ceiling arithmetic.
    let budget = CEILING * ELEM * 2;
    assert!(
        per <= budget,
        "a truncated array reserved its declared count: {per} bytes from a \
         {}-byte header (budget {budget}, declared worst case {})",
        msg.len(),
        DECLARED as usize * ELEM
    );

    // The clamp is a hint, never a length: a well-formed message carrying more
    // than the ceiling still decodes whole.
    let n = CEILING + 1000;
    let mut full = Vec::new();
    put_varint(&mut full, (0u64 << 3) | 3);
    put_varint(&mut full, n as u64);
    for v in 0..n as u64 {
        put_varint(&mut full, v);
    }
    let m = Big::try_decode(&full).expect("a well-formed over-ceiling array must decode");
    assert_eq!(m.arr.len(), n, "the reserve clamp must not truncate the value");
    assert_eq!(m.arr[n - 1], (n - 1) as u64);
    println!("    a well-formed {n}-element array still decodes whole");
}
