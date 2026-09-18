// The std decoder's scope stack is a FIXED array sized from the schema, and a
// wrapper string/blob array grows to what the message carries -- both run, not
// asserted.
//
// The generator tests pin the emitted text: `stack: [_Loc; 4]` for this schema.
// That does not say the number is RIGHT. A stack one entry short is invisible on every message that
// does not reach the deepest frame and then open a sequence there, and the
// symptom when one does is not a crash but the overflow arm: `err`, reported as
// BufferFull, for a message that is perfectly well formed. So the depth is
// pinned from both sides here:
//
//   1. from below: a message that opens an UNKNOWN sequence at every deepest
//      frame kind (a struct field chain, a struct element's inner struct, a
//      string-array element position) -- the longest chain a well-formed message
//      can stack -- must still decode, and the fields written after each
//      skipped subtree must bind where they belong, one-shot and byte by byte.
//   2. from above: an over-index struct element keeps `cur` on its array frame,
//      so a nest inside it can stack past any schema bound. That must surface as
//      InvalidMsg (the over-index verdict, which dominates the overflow's err),
//      never as a panic on an out-of-range index.
//
// And the two allocation properties the change exists for:
//
//   3. a decode that reaches every depth but fills no container allocates
//      NOTHING -- the stack used to be a Vec that allocated at the first
//      sequence_begin of every decode, which on `allow_dynamic: false` was the
//      only heap traffic left. On static storage the whole message is checked.
//   4. on dynamic storage, a wrapper string array declared `count: 1000` that
//      carries ONE element does not hold room for 1000: `count` is a capacity
//      (MESSAGE_SPEC §5.1), and allow_dynamic: true allocates what the message
//      carries, never the declared worst case.
//
// run.sh prepends the `use` line and `const STATIC: bool`.
//SOFAB_IMPORT

use std::alloc::{GlobalAlloc, Layout, System};
use std::sync::atomic::{AtomicUsize, Ordering};

static ALLOCATED: AtomicUsize = AtomicUsize::new(0);

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
static GLOBAL: Counting = Counting;

fn build(f: impl FnOnce(&mut sofab::OStream<'_, sofab::NoFlush>)) -> Vec<u8> {
    let mut buf = vec![0u8; 4096];
    let used = {
        let mut os = sofab::OStream::new(&mut buf);
        f(&mut os);
        os.bytes_used()
    };
    buf.truncate(used);
    buf
}

fn fail(what: &str) -> ! {
    eprintln!("FAIL: {what}");
    std::process::exit(1);
}

// s1.s2.s3 with an unknown subtree (itself nested) opened at s3 -- the deepest
// frame -- and s3.v and the root's tail written after it closes.
fn write_struct_chain(os: &mut sofab::OStream<'_, sofab::NoFlush>) {
    os.write_sequence_begin_lazy(0).unwrap(); // s1
    os.write_sequence_begin_lazy(0).unwrap(); // s2
    os.write_sequence_begin_lazy(0).unwrap(); // s3
    os.write_sequence_begin_lazy(9).unwrap(); // unknown at depth 3
    os.write_unsigned(0, 1).unwrap();
    os.write_sequence_begin_lazy(1).unwrap(); // and deeper inside it
    os.write_unsigned(0, 2).unwrap();
    os.write_sequence_end().unwrap();
    os.write_sequence_end().unwrap();
    os.write_unsigned(0, 42).unwrap(); // s3.v, after the unwind
    os.write_sequence_end().unwrap();
    os.write_sequence_end().unwrap();
    os.write_sequence_end().unwrap();
    os.write_unsigned(1, 7).unwrap(); // tail, at the root
}

fn full_wire() -> Vec<u8> {
    build(|os| {
        write_struct_chain(os);
        // objs[1].inner: an unknown subtree at the element's inner struct.
        os.write_sequence_begin_lazy(2).unwrap();
        os.write_sequence_begin_lazy(1).unwrap();
        os.write_sequence_begin_lazy(0).unwrap();
        os.write_sequence_begin_lazy(5).unwrap();
        os.write_unsigned(0, 1).unwrap();
        os.write_sequence_end().unwrap();
        os.write_unsigned(0, 11).unwrap();
        os.write_sequence_end().unwrap();
        os.write_sequence_end().unwrap();
        os.write_sequence_end().unwrap();
        // rows[0]: a sequence at a string-element position (§7.3, skipped),
        // then the element itself.
        os.write_sequence_begin_lazy(3).unwrap();
        os.write_sequence_begin_lazy(0).unwrap();
        os.write_sequence_begin_lazy(2).unwrap();
        os.write_unsigned(0, 5).unwrap();
        os.write_sequence_end().unwrap();
        os.write_str(1, "ok").unwrap();
        os.write_sequence_end().unwrap();
        os.write_sequence_end().unwrap();
        // names: all five, in order.
        os.write_sequence_begin_lazy(4).unwrap();
        for (i, n) in ["n0", "n1", "n2", "n3", "n4"].iter().enumerate() {
            os.write_str(i as sofab::Id, n).unwrap();
        }
        os.write_sequence_end().unwrap();
    })
}

fn check_full(m: &Deep, how: &str) {
    if m.s1.s2.s3.v != 42 {
        fail(&format!("{how}: s3.v after a skipped subtree at the deepest struct frame: got {}", m.s1.s2.s3.v));
    }
    if m.tail != 7 {
        fail(&format!("{how}: root tail after the struct chain unwound: got {}", m.tail));
    }
    if m.objs.len() != 2 || m.objs[1].inner.w != 11 {
        fail(&format!("{how}: objs[1].inner.w after a skipped subtree at the element's inner struct"));
    }
    if m.rows.len() != 1 || m.rows[0].len() != 2 || m.rows[0][1].as_str() != "ok" {
        fail(&format!("{how}: rows[0][1] after a sequence at a string-element position"));
    }
    if m.names.len() != 5 || m.names[4].as_str() != "n4" {
        fail(&format!("{how}: names"));
    }
}

fn main() {
    // 1. From below: the longest well-formed chain decodes, one-shot and chunked.
    let wire = full_wire();
    let one = match Deep::try_decode(&wire) {
        Ok(m) => m,
        Err(e) => fail(&format!("a well-formed message reaching the schema's full depth was refused: {e:?}")),
    };
    check_full(&one, "try_decode");
    let mut d = DeepDecoder::new();
    for b in &wire {
        if let Err(e) = d.feed(core::slice::from_ref(b)) {
            fail(&format!("byte-by-byte feed refused a well-formed message: {e:?}"));
        }
    }
    let chunked = match d.finish() {
        Ok(m) => m,
        Err(e) => fail(&format!("byte-by-byte finish refused a well-formed message: {e:?}")),
    };
    check_full(&chunked, "Decoder, 1-byte chunks");
    if chunked != one {
        fail("byte-by-byte decode differs from try_decode");
    }

    // 4. A wrapper array grows to what the message carries, not to its count.
    if !STATIC {
        let sparse = build(|os| {
            os.write_sequence_begin_lazy(5).unwrap();
            os.write_str(0, "t").unwrap();
            os.write_sequence_end().unwrap();
        });
        let m = Deep::try_decode(&sparse).unwrap_or_else(|e| fail(&format!("sparse tags refused: {e:?}")));
        if m.tags.len() != 1 || m.tags[0].as_str() != "t" {
            fail("tags[0] after a one-element wrapper array");
        }
        if m.tags.capacity() >= 1000 {
            fail(&format!("a count: 1000 string array carrying one element reserved capacity {}; allow_dynamic: true must not allocate the declared worst case", m.tags.capacity()));
        }
    }

    // 2. From above: over-index element, nested past any schema bound.
    let over = build(|os| {
        os.write_sequence_begin_lazy(2).unwrap();
        os.write_sequence_begin_lazy(5).unwrap(); // id 5 >= count 3
        for _ in 0..8 {
            os.write_sequence_begin_lazy(0).unwrap();
        }
        os.write_unsigned(0, 1).unwrap();
        for _ in 0..8 {
            os.write_sequence_end().unwrap();
        }
        os.write_sequence_end().unwrap();
        os.write_sequence_end().unwrap();
        os.write_unsigned(1, 7).unwrap();
    });
    match Deep::try_decode(&over) {
        Err(DecodeError::Sofab(sofab::Error::InvalidMsg)) => {}
        other => fail(&format!("an over-index element nesting past the stack must be InvalidMsg, got {other:?}")),
    }

    // 3. No heap: the struct chain alone touches no container, so the decode
    // must allocate nothing on either storage; on static storage neither does
    // the whole message.
    let chain = build(|os| write_struct_chain(os));
    let before = ALLOCATED.load(Ordering::Relaxed);
    let m = Deep::try_decode(&chain).unwrap_or_else(|e| fail(&format!("struct chain refused: {e:?}")));
    let spent = ALLOCATED.load(Ordering::Relaxed) - before;
    if spent != 0 || m.s1.s2.s3.v != 42 || m.tail != 7 {
        fail(&format!("a decode that fills no container must not allocate: {spent} bytes"));
    }
    if STATIC {
        let before = ALLOCATED.load(Ordering::Relaxed);
        let m = Deep::try_decode(&wire).unwrap_or_else(|e| fail(&format!("full message refused: {e:?}")));
        let spent = ALLOCATED.load(Ordering::Relaxed) - before;
        if spent != 0 {
            fail(&format!("allow_dynamic: false must decode without the heap: {spent} bytes"));
        }
        check_full(&m, "try_decode (alloc-free)");
    }
    println!("decode stack depth + wrapper growth OK (static={STATIC})");
}
