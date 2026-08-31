// A §7.3-skipped blob is WALKED, never materialised — measured, not asserted.
//
// CORELIB_PLAN §6.2.1 says a skipped field is never capped; §6.6 says the codec
// allocates no payload storage sized from the wire; §6.7.2 says a skipped
// payload is jumped over. Together those say a blob at an id the schema does not
// declare must cost nothing at all.
//
// The reason this is a MEASUREMENT and not a `decode(..).is_ok()` row is that
// dropping a materialised payload also returns Ok. The generated blob callback
// used to feed EVERY delivered payload into `self.acc` — which sizes its buffer
// from the wire `total` and copies the bytes in — and only then dispatch on
// (loc, id) and find no arm. A 1 MiB blob at an unknown id therefore cost 1 MiB
// for a field nobody reads, and every existing "skipped field decodes COMPLETE"
// row passed while it did. This counts every byte the process allocates during
// the decode and requires it to be a small fraction of the skipped payload.
//
// The crate this runs against is built with max_dyn_blob_len: 8, so the row
// carries the §6.2.1 half too: a 1 MiB blob at an unknown id is over the cap by
// five orders of magnitude and must STILL decode — a skipped field is never
// capped — and must still allocate nothing.
//
// ARCHITECTURE §9.5.2 exempts Rust std from moving the receiver CAP into the
// corelib. It exempts nothing from this: where the comparison lives is a
// separate question from whether a skipped field is materialised at all.
//SOFAB_IMPORT

use std::alloc::{GlobalAlloc, Layout, System};
use std::sync::atomic::{AtomicUsize, Ordering};

static ALLOCATED: AtomicUsize = AtomicUsize::new(0);

/// The system allocator with a running total of every byte handed out. Only
/// growth is counted: a decode that reuses a buffer it already owns is not
/// allocating payload storage, and a free-then-reallocate would otherwise net to
/// zero and hide exactly the copy this row exists to catch.
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
    const N: usize = 1 << 20; // 1 MiB payload
    const SKIPPED_ID: u64 = 7; // no field of `dyn` declares it

    // fixlen header (id << 3 | 2), fixlen word (len << 3 | 3 == blob), payload.
    let mut msg = Vec::with_capacity(N + 32);
    put_varint(&mut msg, (SKIPPED_ID << 3) | 2);
    put_varint(&mut msg, ((N as u64) << 3) | 3);
    msg.resize(msg.len() + N, b'a');

    let m = Dyn::try_decode(&msg).expect("a skipped 1 MiB blob must decode Ok, cap or no cap");
    assert!(m.b.is_empty(), "the skipped payload landed in the declared blob b");
    assert!(m.s.is_empty(), "the skipped payload landed in the declared string s");

    let reps = 16usize;
    let before = ALLOCATED.load(Ordering::Relaxed);
    for _ in 0..reps {
        let m = Dyn::try_decode(&msg).expect("decode");
        std::hint::black_box(&m);
    }
    let per = (ALLOCATED.load(Ordering::Relaxed) - before) / reps;
    println!("    a skipped 1 MiB blob allocates {per} bytes per decode");

    assert!(
        per <= N / 8,
        "a skipped blob was materialised: {per} bytes allocated for a payload of {N}"
    );
}
