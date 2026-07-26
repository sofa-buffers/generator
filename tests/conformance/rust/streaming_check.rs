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

fn main() {
    let mut m = Myfirstmessage::default();
    m.somei8 = -5;
    m.somebool = true;
    m.someu64 = u64::MAX; // widest varint
    m.somefp32 = 2.5;

    // ---- 1. streaming encode is byte-identical -------------------------

    let one_shot = m.encode();
    assert!(!one_shot.is_empty(), "message encoded to nothing");

    // 7 bytes: far smaller than any field, so the sink fires mid-value and the
    // stream has to carry its buffer state across flushes.
    let mut streamed: Vec<u8> = Vec::new();
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

    println!(
        "streaming: encode byte-identical through a 7-byte buffer; decode value-identical \
at 7 chunk sizes; of {} truncations {incompletes} were rejected as Incomplete and \
{completions} decoded cleanly on a field boundary",
        one_shot.len() - 1
    );
}
