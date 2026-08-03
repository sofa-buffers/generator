// Streaming check for the PURE heapless no_std profile (allow_dynamic: false).
//
// The main streaming_check.rs runs against the alloc leg. This one exists
// because the heapless profile has a different `acc` — the buffer that
// reassembles a string/blob payload split across feed chunks is a
// `heapless::Vec` with fixed capacity here, not a growable `Vec`. That is a
// distinct code path with a distinct failure mode (a push past capacity sets the
// sticky `err` flag instead of allocating), and compiling it is not the same as
// running it.
//
// The property is the same one the alloc leg asserts: streaming must be
// indistinguishable from the one-shot path.

use sofabuffers_generated::*;

fn main() {
    // A string long enough that it cannot land inside a single chunk at any of
    // the sizes below — the whole point is to make `acc` carry a partial payload
    // from one feed to the next, repeatedly.
    let text: heapless::String<4096> = core::iter::repeat('x')
        .take(600)
        .collect::<std::string::String>()
        .as_str()
        .try_into()
        .expect("fits the schema maxlen");

    let mut m = Vecs::default();
    m.a = text;

    let one_shot = m.encode();
    let expect = Vecs::try_decode(&one_shot).expect("one-shot decode failed");

    // 1. streaming encode is byte-identical, through a buffer far smaller than
    //    the payload.
    let mut streamed: std::vec::Vec<u8> = std::vec::Vec::new();
    {
        let mut scratch = [0u8; 7];
        let mut os = sofab::OStream::with_flush(&mut scratch, 0, |d: &[u8]| {
            streamed.extend_from_slice(d)
        });
        m.serialize(&mut os);
        os.flush();
    }
    assert_eq!(
        &one_shot[..],
        &streamed[..],
        "heapless: streaming encode differs from encode()"
    );

    // 2. chunked decode is value-identical. At size 1 the 600-byte payload is
    //    delivered in 600 separate feeds, so `acc` has to hold and extend a
    //    partial string across every one of them.
    for size in [1usize, 2, 3, 5, 16, 64, 4096] {
        let mut dec = Vecs::decoder();
        for chunk in one_shot.chunks(size) {
            match dec.feed(chunk) {
                Ok(()) | Err(sofab::Error::Incomplete) => continue,
                Err(e) => panic!("heapless chunk size {size}: feed failed: {e:?}"),
            }
        }
        let got = dec.finish().expect("heapless finish failed");
        assert!(
            got == expect,
            "heapless chunk size {size}: decoded value differs from the one-shot decode"
        );
        assert_eq!(got.a.len(), 600, "heapless chunk size {size}: payload length");
    }

    // 3. a wrapper array of heapless strings: element boundaries and payload
    //    boundaries both fall inside chunks.
    // `a` is `count: 8` — a CAPACITY, not a length (S3), so its Default is the
    // EMPTY array: the heapless::Vec holding it has capacity 8 and length 0, which
    // is exactly how a fixed-storage target expresses 0..N. Fill it by pushing.
    let mut arr = Vecsa::default();
    assert_eq!(arr.a.len(), 0, "a count: 8 wrapper array defaults to empty");
    for i in 0..8 {
        arr.a
            .push(
                std::format!("element-{i:03}")
                    .as_str()
                    .try_into()
                    .expect("fits the element maxlen"),
            )
            .expect("fits the schema count");
    }
    let wire = arr.encode();
    let want = Vecsa::try_decode(&wire).expect("one-shot decode failed");

    for size in [1usize, 3, 7, 16] {
        let mut dec = Vecsa::decoder();
        for chunk in wire.chunks(size) {
            match dec.feed(chunk) {
                Ok(()) | Err(sofab::Error::Incomplete) => continue,
                Err(e) => panic!("heapless array chunk size {size}: feed failed: {e:?}"),
            }
        }
        let got = dec.finish().expect("heapless array finish failed");
        assert!(
            got == want,
            "heapless array chunk size {size}: decoded value differs from the one-shot decode"
        );
        assert_eq!(got.a.len(), 8, "heapless array chunk size {size}: element count");
    }

    // 4. a skipped subtree deeper than the scope stack (generator#283). This is
    //    the profile the defect lived on: the stack is a fixed-capacity
    //    heapless::Vec sized from the SCHEMA (here 4 entries), while nesting depth
    //    comes from the WIRE — an unknown sequence may nest up to MAX_DEPTH (255)
    //    and must be skipped, not overrun the stack. Levels inside a skipped
    //    subtree are counted rather than stacked, and that counter is persistent
    //    state, so a chunk boundary may fall anywhere inside the unwind.
    //
    //    Wire: open the wrapper array (id 0), nest 40 unknown sequences (id 60)
    //    and close them all, then — back at the array scope — element 0 = "hi"
    //    (fixlen: id 0, str subtype, 2 bytes), then close.
    let mut deep: std::vec::Vec<u8> = std::vec![0x06];
    deep.extend(core::iter::repeat([0xe6, 0x03]).take(40).flatten());
    deep.extend(core::iter::repeat(0x07u8).take(40));
    deep.extend([0x02, 0x12, b'h', b'i', 0x07]);
    for size in [1usize, 3, 7, 64] {
        let mut dec = Vecsa::decoder();
        for chunk in deep.chunks(size) {
            match dec.feed(chunk) {
                Ok(()) | Err(sofab::Error::Incomplete) => continue,
                Err(e) => panic!("heapless deep-nest chunk size {size}: feed failed: {e:?}"),
            }
        }
        let got = dec.finish().expect("heapless deep-nest finish failed");
        assert!(
            got.a.len() == 1 && got.a[0].as_str() == "hi",
            "heapless chunk size {size}: the element after a 40-deep unknown-sequence \
unwind was lost (got {} element(s))",
            got.a.len()
        );
    }

    println!(
        "streaming (pure heapless): {}-byte payload byte-identical through a 7-byte buffer; \
value-identical at 7 chunk sizes; 8-element wrapper array at 4 chunk sizes; a 40-deep \
skipped subtree unwinds correctly at 4 chunk sizes",
        one_shot.len()
    );
}
