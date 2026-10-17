//SOFAB_IMPORT
// try_decode(data, &mut out) keeps the storage a reused destination holds
// (generator#755), measured with a counting global allocator rather than
// asserted from the generated text.
//
// run.sh puts `type M = <Message>;` in front of this file and pipes the bench
// payload (JSON) to it; the message is examples/messages/realworld/
// vehicle_telemetry.yaml or tests/bench/schema/unbounded_ingest.yaml, the two
// bench schemas, which between them hold every kind of storage a reset keeps: a
// string, a blob, native arrays, and wrapper arrays of strings, blobs, rows and
// structs.
//
// What it pins:
//   1. the steady state: a destination that already held the message decodes it
//      again without a single allocation -- the issue's 32/38 allocations per
//      decode, and the whole point of the signature;
//   2. shrink then grow: the empty message (every field at its default) costs no
//      allocation; growing back costs at most what a fresh destination does
//      (the elements past an array's new length were dropped, as the corelib's
//      truncate drops them), and the decode after it is 0 again;
//   3. a refusal: the full message with a malformed tail is refused, allocates
//      nothing, and leaves the fields decoded before the error (MESSAGE_SPEC
//      §2: what a fresh decode of the bytes before the tail reads);
//   4. every result equals a fresh decode of the same bytes, so the reuse
//      changes what is allocated and nothing else.
use std::alloc::{GlobalAlloc, Layout, System};
use std::io::Read;
use std::sync::atomic::{AtomicUsize, Ordering};

struct Counting;

static ALLOCS: AtomicUsize = AtomicUsize::new(0);

unsafe impl GlobalAlloc for Counting {
    unsafe fn alloc(&self, l: Layout) -> *mut u8 {
        ALLOCS.fetch_add(1, Ordering::Relaxed);
        System.alloc(l)
    }
    unsafe fn dealloc(&self, p: *mut u8, l: Layout) {
        System.dealloc(p, l)
    }
    unsafe fn realloc(&self, p: *mut u8, l: Layout, n: usize) -> *mut u8 {
        ALLOCS.fetch_add(1, Ordering::Relaxed);
        System.realloc(p, l, n)
    }
}

#[global_allocator]
static COUNTING: Counting = Counting;

fn allocs(f: impl FnOnce()) -> usize {
    let before = ALLOCS.load(Ordering::Relaxed);
    f();
    ALLOCS.load(Ordering::Relaxed) - before
}

fn fail(msg: &str) -> ! {
    eprintln!("FAIL: {msg}");
    std::process::exit(1);
}

fn main() {
    let mut json = String::new();
    std::io::stdin().read_to_string(&mut json).unwrap();
    let msg: M = serde_json::from_str(&json).unwrap_or_else(|e| fail(&format!("payload: {e}")));
    let full = msg.encode().unwrap_or_else(|e| fail(&format!("encode: {e:?}")));
    let fresh = M::decode(&full);
    if fresh != msg {
        fail("the payload does not survive a fresh round trip");
    }

    let mut out = M::default();
    let first = allocs(|| M::try_decode(&full, &mut out).unwrap_or_else(|e| fail(&format!("decode #1: {e:?}"))));
    if out != fresh {
        fail("decode #1 into a fresh destination differs from decode()");
    }

    // 1. the steady state.
    for n in 2..=3 {
        let a = allocs(|| M::try_decode(&full, &mut out).unwrap_or_else(|e| fail(&format!("decode #{n}: {e:?}"))));
        if a != 0 {
            fail(&format!("decode #{n} into a destination that held the same message allocated {a} times (decode #1: {first})"));
        }
        if out != fresh {
            fail(&format!("decode #{n} into a reused destination differs from a fresh decode"));
        }
    }

    // 2. shrink to the empty message, then grow back. Shrinking allocates
    // nothing. It does give storage back where the corelib's protocol says it
    // must: SeqVec::truncate drops the elements past an array's new length and
    // keeps only the array's own buffer, so growing back pays for those elements
    // again -- never for more than a decode into a fresh destination does -- and
    // the decode after it is the steady state again.
    let a = allocs(|| M::try_decode(&[], &mut out).unwrap_or_else(|e| fail(&format!("empty: {e:?}"))));
    if out != M::default() {
        fail("the empty message decoded into a reused destination is not the default message");
    }
    if a != 0 {
        fail(&format!("the empty message decoded into a reused destination allocated {a} times"));
    }
    let b = allocs(|| M::try_decode(&full, &mut out).unwrap_or_else(|e| fail(&format!("regrow: {e:?}"))));
    if b > first {
        fail(&format!("growing back from the empty message allocated {b} times, a fresh destination {first}"));
    }
    if out != fresh {
        fail("the regrown destination differs from a fresh decode");
    }
    let c = allocs(|| M::try_decode(&full, &mut out).unwrap_or_else(|e| fail(&format!("after regrow: {e:?}"))));
    if c != 0 {
        fail(&format!("the decode after growing back allocated {c} times"));
    }

    // 3. a refusal: an 11-byte field header is INVALID in any schema (§4.1).
    let mut bad = full.clone();
    bad.extend_from_slice(&[0x80; 10]);
    bad.push(0x01);
    let mut verdict = Ok(());
    let d = allocs(|| verdict = M::try_decode(&bad, &mut out));
    if verdict.is_ok() {
        fail("a malformed tail was accepted");
    }
    if d != 0 {
        fail(&format!("a refused decode into a reused destination allocated {d} times"));
    }
    if out != fresh {
        fail("a refused decode left something other than the fields decoded before the error");
    }

    println!(
        "==> reused destination: decode #1 {first} allocations, steady state 0, empty 0, regrow {b}, refused 0 ({} bytes)",
        full.len()
    );
}
