// MESSAGE_SPEC §7.4 — a field id REPEATED inside one scope — measured at every
// array position example.yaml offers, on a message this file builds itself.
//
// Nothing in the shared vectors reaches a repeated id, and nothing can: the
// clause opens by saying such an encoding is not well formed and producers MUST
// NOT emit it. It is a DECODER obligation only, so a vector would have to be
// hand-forged bytes that no encoder in the family will ever produce — which is
// the same reason `tests/conformance/lib/check_growth.py` builds its own message
// instead of reading one. Eleven green conformance suites therefore never saw
// generator#509, and `check_vectors_decode.py` says so in as many words.
//
// The rule has two halves, and the whole value of this check is that it pins
// BOTH, so a fix for one cannot quietly break the other:
//
//   a re-opened SEQUENCE continues its scope  -> struct/union elements MERGE,
//     and children set by an earlier opening whose ids do not recur are retained;
//   an ARRAY WRAPPER is the exception          -> it *is* the value of its array
//     field, so a later occurrence REPLACES it whole.
//
// generator#509 was the second half missing for one position: a nested NATIVE
// row. `array_begin` grew the outer container to the row id and recorded the
// index but never reset the row, so the elements that followed pushed on top of
// the previous occurrence's. Measured on `corelib: rs` before the fix, case 1
// below decoded as
//
//     somematrix = [[1, 2, 3, 4, 5]]      row 0 len 5
//
// against [[4, 5]] from go (measured the same way), csharp, java and zig. Case 2
// is the other half of the same defect: somematrix declares an inner `count: 4`
// and the header check is per occurrence, so merging also carried a row past its
// own declared capacity — two legal occurrences of 3 and 4 adding up to 7.
// Clearing the row on open restores the value AND re-arms the bound.
//
// The rows are chosen so a MERGE and a REPLACE cannot produce the same answer:
// the second occurrence is always SHORTER than the first, so an implementation
// that appends is caught by the length even where the tail values coincide.
//SOFAB_IMPORT

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

fn decode(wire: &[u8]) -> Myfirstmessage {
    Myfirstmessage::try_decode(wire).expect("a repeated id is well-defined, never INVALID (§7.4)")
}

fn fail(what: &str, got: String, want: &str) -> ! {
    eprintln!("FAIL: §7.4 {what}\n  got  {got}\n  want {want}");
    std::process::exit(1);
}

fn main() {
    // 1. THE ARRAY WRAPPER, native row (fkNestedNative) — generator#509.
    //    somematrix is id 24, outer count 2, inner count 4. Two occurrences of
    //    row id 0, the second one shorter.
    let wire = build(|os| {
        os.write_sequence_begin_lazy(24).unwrap();
        os.write_array_unsigned(0, &[1u32, 2, 3]).unwrap();
        os.write_array_unsigned(0, &[4u32, 5]).unwrap();
        os.write_sequence_end().unwrap();
    });
    let m = decode(&wire);
    let rows: Vec<Vec<u32>> = m.somematrix.iter().map(|r| r.iter().copied().collect()).collect();
    if rows != vec![vec![4u32, 5]] {
        fail(
            "a repeated NATIVE ROW id must replace the row, not merge into it",
            format!("somematrix = {rows:?}"),
            "somematrix = [[4, 5]]",
        );
    }

    // 1b. ...and the same bytes CHUNKED. The fix is a DESTRUCTIVE clear, and its
    //     safety rests entirely on `array_begin` firing exactly ONCE per array
    //     field. That holds in both corelibs today - corelib-rs resumes a row
    //     without re-parsing its header, and corelib-rs-no-std's byte state
    //     machine reaches on_array_count once per header word - but nothing in
    //     this repo pinned it. If a corelib ever re-announced an array on resume,
    //     a chunked repeated-row message would silently drop every chunk but the
    //     last and all eleven suites would stay green: the "a corelib change
    //     breaks codegen silently" shape. check_chunk_invariance.py cannot reach
    //     it either, because it replays shared vectors and no shared vector
    //     carries a repeated id. So the chunked path is asserted here, against the
    //     one-shot answer above.
    for size in [1usize, 2, 3, 5, 7] {
        let mut dec = Myfirstmessage::decoder();
        for chunk in wire.chunks(size) {
            // Neither Status means "done": the format has no top-level end
            // marker, so a chunk ending on a field boundary answers Complete
            // while more of the message follows. Only Err is fatal.
            if let Err(e) = dec.feed(chunk) {
                panic!("chunk size {size}: feed failed: {e:?}");
            }
        }
        let got = dec.finish().expect("chunked decode of a repeated id must not be INVALID");
        let rows: Vec<Vec<u32>> =
            got.somematrix.iter().map(|r| r.iter().copied().collect()).collect();
        if rows != vec![vec![4u32, 5]] {
            fail(
                "a repeated NATIVE ROW id must replace the row on the CHUNKED path too",
                format!("chunk size {size}: somematrix = {rows:?}"),
                "somematrix = [[4, 5]] at every chunk size",
            );
        }
    }

    // 2. ...and the bound the merge used to slip past. Each occurrence is legal
    //    on its own (3 and 4 are both <= the inner count 4); appended they are 7,
    //    which no single occurrence could have declared.
    let m = decode(&build(|os| {
        os.write_sequence_begin_lazy(24).unwrap();
        os.write_array_unsigned(0, &[1u32, 2, 3]).unwrap();
        os.write_array_unsigned(0, &[4u32, 5, 6, 7]).unwrap();
        os.write_sequence_end().unwrap();
    }));
    let len = m.somematrix.first().map(|r| r.len()).unwrap_or(0);
    if len != 4 {
        fail(
            "a repeated row must not accumulate past its declared inner count",
            format!("somematrix row 0 len = {len}"),
            "somematrix row 0 len = 4 (declared count: 4)",
        );
    }

    // 3. THE ARRAY WRAPPER, leaf native array. Same rule at the top level, where
    //    the arm has always cleared — the control that says the row arm was the
    //    outlier and not the whole backend. someuintarray is id 15, count 4.
    let m = decode(&build(|os| {
        os.write_array_unsigned(15, &[1u32, 2, 3, 4]).unwrap();
        os.write_array_unsigned(15, &[9u32]).unwrap();
    }));
    let got: Vec<u32> = m.someuintarray.iter().copied().collect();
    if got != vec![9u32] {
        fail(
            "a repeated LEAF array id must replace the array",
            format!("someuintarray = {got:?}"),
            "someuintarray = [9]",
        );
    }

    // 4. THE ARRAY WRAPPER, string element inside a wrapper sequence. The element
    //    is a leaf VALUE at its id, so the last one wins. somestringarray is
    //    id 18, count 5, maxlen 16.
    let m = decode(&build(|os| {
        os.write_sequence_begin_lazy(18).unwrap();
        os.write_str(0, "first").unwrap();
        os.write_str(0, "last").unwrap();
        os.write_sequence_end().unwrap();
    }));
    let got: Vec<&str> = m.somestringarray.iter().map(|s| s.as_str()).collect();
    if got != vec!["last"] {
        fail(
            "a repeated STRING ELEMENT id must replace the element",
            format!("somestringarray = {got:?}"),
            r#"somestringarray = ["last"]"#,
        );
    }

    // 5. THE OTHER HALF: a re-opened SEQUENCE continues its scope. somestructarray
    //    is id 23, elements are {x: i32 @0, y: i32 @1}. Element 0 is opened twice;
    //    the second opening sets only x, so y MUST survive from the first. A
    //    backend that "fixed" §7.4 by resetting every repeated element id would
    //    zero y here — which is why this case sits beside the four above rather
    //    than in a file of its own.
    let m = decode(&build(|os| {
        os.write_sequence_begin_lazy(23).unwrap();
        os.write_sequence_begin_lazy(0).unwrap();
        os.write_signed(0, 11).unwrap();
        os.write_signed(1, 22).unwrap();
        os.write_sequence_end().unwrap();
        os.write_sequence_begin_lazy(0).unwrap();
        os.write_signed(0, 33).unwrap();
        os.write_sequence_end().unwrap();
        os.write_sequence_end().unwrap();
    }));
    let got: Vec<(i32, i32)> = m.somestructarray.iter().map(|e| (e.x, e.y)).collect();
    if got != vec![(33i32, 22i32)] {
        fail(
            "a re-opened STRUCT element continues its scope: unrecurring children are retained",
            format!("somestructarray = {got:?}"),
            "somestructarray = [(33, 22)]",
        );
    }

    println!("§7.4 repeated id: wrappers replace, scopes merge — OK");
}
