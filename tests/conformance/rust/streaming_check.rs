// Behavioural check for the streaming API (generator PR #242).
//
// The generator tests assert that `serialize<F: Flush>`, `decoder()` and
// `feed`/`finish` appear in the output. That pins the shape and nothing else —
// it would pass just as happily against a decoder that dropped every second
// chunk. This runs them.
//
// The property worth checking is that **streaming is indistinguishable from the
// one-shot path**:
//
//   1. `serialize` through a sink must produce byte-for-byte what `encode()`
//      produces, no matter how small the scratch buffer is.
//   2. the incremental decoder must produce the same value as `try_decode`,
//      no matter where the chunk boundaries fall.
//   3. a truncated stream must be rejected, not returned half-filled.
//
// Point 2 is the one that finds real bugs: at a chunk size of 1 every varint,
// every string and every array element is split across feeds, so any parse
// state the decoder fails to carry between calls shows up immediately.
//
// The `use` line is supplied by run.sh — the std project is a binary crate that
// includes `message.rs` as a module, the no_std project is a library.

//SOFAB_IMPORT

#[allow(deprecated)]
fn main() {
    // Every field SHAPE has to be present, not just a few scalars. Chunked
    // feeding exercises decoder state that a scalar-only message never reaches:
    //
    //   acc          reassembles a string/blob payload split across feeds — its
    //                only reason to exist, and untouched unless a string or blob
    //                actually carries bytes
    //   stack/cur    the location stack, pushed and popped around nested
    //                sequences; a chunk boundary can fall between the push and
    //                the pop
    //   afill        native-array fill progress, which must survive a boundary
    //                falling between two elements
    //
    // The strings and blobs below are deliberately long: at every chunk size
    // tested they are guaranteed to straddle a boundary rather than land inside
    // one chunk by luck.
    let mut m = Myfirstmessage::default();

    // scalars, each at a width that makes its varint as wide as it gets
    m.someu8 = u8::MAX;
    m.someu16 = u16::MAX;
    m.someu32 = u32::MAX;
    m.someu64 = u64::MAX - 1; // the schema default IS u64::MAX, so shift off it
    m.somei8 = -5;
    m.somei16 = i16::MIN;
    m.somei32 = i32::MIN;
    m.somei64 = i64::MIN;
    m.somefp32 = 2.5;
    m.somefp64 = -1.0e300;
    m.somebool = !m.somebool;
    m.someenum = 33;
    m.somebitfield = 2;

    // long payloads: these are what acc exists for
    // At their schema maxlen (50 and 16) -- the longest the format permits here,
    // and long enough to straddle a boundary at every chunk size below 50.
    // Exceeding the bound is not a bigger test, it is a different one: §7.1 makes
    // an over-maxlen payload INVALID, so the decode would be rejected outright.
    m.somestring = "0123456789-0123456789-0123456789-0123456789-01234".into();
    debug_assert!(m.somestring.len() <= 50);
    m.someblob = (0u8..16).collect();

    // native arrays: `count: N` is a CAPACITY, not a length (MESSAGE_SPEC S3), so
    // a fresh field holds only its declared default -- clear that and fill each
    // array to its full N, which is what makes the encoded message the widest one
    // this schema allows.
    m.someuintarray.clear();
    for i in 0..4u32 {
        m.someuintarray.push((i + 1) * 100_000);
    }
    m.someintarray.clear();
    for i in 0..5i32 {
        m.someintarray.push(-((i + 1) * 100_000));
    }
    m.somefloatarray.clear();
    for i in 0..3 {
        m.somefloatarray.push(i as f32 + 0.5);
    }
    m.someenumarray.clear();
    for i in 0..4 {
        m.someenumarray.push((i % 2) as i8);
    }
    m.someboolarray.clear();
    for _ in 0..8 {
        m.someboolarray.push(true);
    }
    m.somebitfieldarray.clear();
    for i in 0..3u8 {
        m.somebitfieldarray.push(i | 1);
    }

    // wrapper-sequence arrays: their elements are child fields keyed by index, so
    // a boundary can fall between two elements or inside one element's payload.
    // Filled to N for the same reason as the native arrays above -- and no
    // further: an element id >= N is INVALID (S5.1/S7), which is what the outer
    // count: 2 on `somematrix` pins.
    m.somestringarray.clear();
    for i in 0..5 {
        m.somestringarray.push(format!("elem-{i:08}")); // 13 <= maxlen 16
    }
    m.someblobarray.clear();
    for i in 0..3 {
        m.someblobarray.push(vec![i as u8; 8]); // 8 == maxlen 8
    }
    m.somematrix.clear();
    for i in 0..2u32 {
        m.somematrix.push(vec![i * 11, i * 22, i * 33]);
    }

    // nested sequence: a chunk boundary can fall between its begin and end
    m.somestruct.nestedint = 7;
    m.somestruct.nestedstring = "nested-string-straddles".into();
    m.somestruct.nestedstruct.deepint = -99;
    m.someunion.option1 = 4242;

    // ---- 1. streaming encode is byte-identical -------------------------

    let one_shot = m.encode();
    assert!(!one_shot.is_empty(), "message encoded to nothing");

    // 7 bytes: far smaller than any field, so the sink fires mid-value and the
    // stream has to carry its buffer state across flushes.
    let mut streamed: Vec<u8> = Vec::new();
    {
        let mut scratch = [0u8; 7];
        // with_flush reports its capacity precondition as a status in BOTH Rust
        // corelibs -- this check runs against either, and the two ports share the
        // spelling precisely so one source can. 7 bytes clears MIN_OUTPUT_BUFFER.
        let mut os = sofab::OStream::with_flush(&mut scratch, 0, |d: &[u8]| {
            streamed.extend_from_slice(d)
        })
        .expect("7 bytes is above MIN_OUTPUT_BUFFER");
        m.serialize(&mut os);
        os.flush();
    }
    assert_eq!(
        &one_shot[..],
        &streamed[..],
        "streaming encode differs from encode(): {} vs {} bytes",
        one_shot.len(),
        streamed.len()
    );

    // ---- 2. chunked decode is value-identical --------------------------

    let expect = Myfirstmessage::try_decode(&one_shot).expect("one-shot decode failed");

    for size in [1usize, 2, 3, 5, 16, 64, 4096] {
        let mut dec = Myfirstmessage::decoder();

        // Feed everything. Neither Ok nor Incomplete means "done" here: the wire
        // format has no top-level end marker, so a chunk that happens to end on a
        // field boundary returns Ok even though more of the message follows.
        // Stopping at the first Ok is exactly the mistake this loop must not make
        // -- at chunk size 1 that would truncate after the first complete field.
        for chunk in one_shot.chunks(size) {
            match dec.feed(chunk) {
                Ok(()) | Err(sofab::Error::Incomplete) => continue,
                Err(e) => panic!("chunk size {size}: feed failed: {e:?}"),
            }
        }

        // The caller's framing says the input is over; finish gives the verdict.
        let got = dec.finish().expect("finish failed");
        assert!(
            got == expect,
            "chunk size {size}: decoded value differs from the one-shot decode"
        );
    }

    // ---- 3. a cut inside a field is rejected, not returned half-filled ---

    // Truncation is NOT automatically an error. The format has no top-level end
    // marker and no required fields, so a message cut on a field boundary is a
    // valid, shorter message -- and cutting off a trailing run of EMPTY sequence
    // frames (which every struct/union/wrapper-array field emits whether or not
    // it carries anything) does not even change the decoded value.
    //
    // What must hold is narrower: a cut INSIDE a field leaves the decoder
    // half-read, and finish() must say so rather than hand over the partial
    // value. That is what the end-of-input probe inside finish() is for; the
    // counter proves the path is reached instead of being silently unreachable.
    let mut incompletes = 0;
    let mut completions = 0;
    for cut in 1..one_shot.len() {
        let mut dec = Myfirstmessage::decoder();
        let _ = dec.feed(&one_shot[..cut]);
        match dec.finish() {
            Err(sofab::Error::Incomplete) => incompletes += 1,
            Ok(_) => completions += 1,
            Err(e) => panic!("truncating at {cut} reported {e:?}"),
        }
    }
    assert!(
        incompletes > 0,
        "no truncation reported Incomplete -- finish()'s end-of-input probe never fires, \
so a half-read field would be returned as a value"
    );
    assert!(
        completions > 0,
        "every truncation was Incomplete -- a cut on a field boundary should decode"
    );

    // ---- 4. a skipped subtree survives a chunk boundary (generator#283) ---

    // The scope tracker carries a depth counter for sequences opened inside a
    // skipped subtree (they are counted, not stacked, so the stack stays bounded
    // by the schema's frame count). Like `stack`/`cur`/`acc` it is persistent
    // state, so a chunk boundary may fall anywhere inside the unwind -- and if it
    // were not carried across feeds, the pops would resume against the wrong
    // depth and the field written AFTER the unwind would bind nowhere.
    //
    // Wire: open somestruct (id 20), nest 40 unknown sequences (id 60) and close
    // them all, then -- back at somestruct scope -- nestedint = 42, then close.
    let mut deep: Vec<u8> = vec![0xa6, 0x01];
    deep.extend(std::iter::repeat([0xe6, 0x03]).take(40).flatten());
    deep.extend(std::iter::repeat(0x07u8).take(40));
    deep.extend([0x00, 0x2a, 0x07]);
    for size in [1usize, 2, 3, 5, 16, 64] {
        let mut dec = Myfirstmessage::decoder();
        for chunk in deep.chunks(size) {
            match dec.feed(chunk) {
                Ok(()) | Err(sofab::Error::Incomplete) => continue,
                Err(e) => panic!("deep-nest chunk size {size}: feed failed: {e:?}"),
            }
        }
        let got = dec.finish().expect("deep-nest finish failed");
        assert!(
            got.somestruct.nestedint == 42,
            "chunk size {size}: the field after a 40-deep unknown-sequence unwind was lost"
        );
    }

    println!(
        "streaming: encode byte-identical through a 7-byte buffer; decode value-identical \
at 7 chunk sizes; a 40-deep skipped subtree unwinds correctly at 6 chunk sizes; of {} \
truncations {incompletes} were rejected as Incomplete and \
{completions} decoded cleanly on a field boundary",
        one_shot.len() - 1
    );
}
