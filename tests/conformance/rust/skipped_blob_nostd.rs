// The footprint half of the skipped-blob rule, and the half that is not a
// measurement but a hard failure.
//
// corelib-rs-no-std's PayloadAcc is a FIXED arena: `feed(total, ..)` answers
// `Err(Error::Argument)` the moment `total` exceeds its capacity, which the
// generated crate turns into a decode error. The capacity comes from the schema's
// largest bounded payload -- here 8 bytes.
//
// So while the blob callback fed every delivered payload into that arena before
// resolving a destination, a blob at an id the schema does not declare was not
// merely copied for nothing: anything larger than the arena FAILED THE WHOLE
// DECODE. A sender adding a field the receiver has not been rebuilt for is the
// ordinary forward-compatibility case MESSAGE_SPEC §7.3 exists to make safe, and
// on this profile it was a denial of service in one field.
//
// The std leg measures allocation because there the cost is only waste. Here the
// verdict itself is the test: this message must decode.
//SOFAB_IMPORT

fn put_varint(out: &mut Vec<u8>, mut v: u64) {
    while v >= 0x80 {
        out.push((v as u8 & 0x7f) | 0x80);
        v >>= 7;
    }
    out.push(v as u8);
}

fn main() {
    const N: usize = 1024; // 128x the 8-byte accumulator this schema sizes
    const SKIPPED_ID: u64 = 7; // no field of `sb` declares it

    // fixlen header (id << 3 | 2), fixlen word (len << 3 | 3 == blob), payload.
    let mut msg = Vec::new();
    put_varint(&mut msg, (SKIPPED_ID << 3) | 2);
    put_varint(&mut msg, ((N as u64) << 3) | 3);
    msg.resize(msg.len() + N, b'a');

    let m = Sb::try_decode(&msg)
        .expect("a skipped blob larger than the fixed accumulator must still decode");
    assert!(m.b.is_empty(), "the skipped payload landed in the declared blob b");
    assert!(m.s.is_empty(), "the skipped payload landed in the declared string s");
    println!("    a {N}-byte blob skipped past an 8-byte accumulator decodes Ok");
}
