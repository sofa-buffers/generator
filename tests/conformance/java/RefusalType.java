// Not generated: dropped into the generated project by the conformance harness,
// which builds it into the same jar as the message classes.
//
// The convenience entry points encode() and decode() report a refusal through
// the CHECKED SofabException, like serialize/encodeTo/tryDecode beside them
// (generator#748). They used to catch it and rethrow a bare
// java.lang.RuntimeException, which cost a caller two things: the SofabError sat
// one getCause() away, and a refusal became indistinguishable from a genuine bug
// raised in the same call -- an NPE arrives as a RuntimeException too.
//
// Half of this check is the COMPILE, not the run. Java rejects `catch (X)` for a
// checked X the try block cannot throw, so each catch clause below only compiles
// against a method that declares it: this file would not build against the
// wrapping shape at all. The run adds what the signature cannot say -- that the
// refusal really arrives here as a SofabException carrying its own SofabError,
// against the real corelib, rather than as something that merely satisfies the
// clause.
package message;

import java.io.IOException;
import org.sofabuffers.sofab.SofabError;
import org.sofabuffers.sofab.SofabException;

public final class RefusalType {

    private static int failures = 0;

    private static void check(String what, SofabError want, SofabError got) {
        if (want != got) {
            System.out.println("FAIL: " + what + ": want " + want + ", got " + got);
            failures++;
        }
    }

    public static void main(String[] args) throws IOException {
        // ---- encode(): a value past the declared maxlen ---------------------
        // This `catch` is the compile-time assertion: javac rejects a catch of a
        // checked type the try block cannot throw, so it only builds against an
        // encode() that declares SofabException or a supertype of it. Against the
        // old unchecked shape the file does not compile. The IOException arm below
        // is the report for a refusal that somehow is not a SofabException.
        Refuse m = new Refuse();
        m.s = "0123456789";         // maxlen: 8
        SofabError enc = null;
        try {
            m.encode();
            System.out.println("FAIL: encode() of an over-maxlen string returned bytes");
            failures++;
        } catch (SofabException e) {
            enc = e.error();
        } catch (IOException e) {
            System.out.println("FAIL: encode() refused with a plain IOException: " + e);
            failures++;
        }
        if (enc != null) {
            check("encode() over maxlen", SofabError.ARGUMENT, enc);
        }

        // ---- decode(): a string on the wire past the declared maxlen --------
        // id 0, payload wire type; one final chunk of 10 bytes ((10 << 3) | 2).
        byte[] over = new byte[] {0x02, 0x52, '0', '1', '2', '3', '4', '5', '6', '7', '8', '9'};
        try {
            Refuse.decode(over);
            System.out.println("FAIL: decode() of an over-maxlen string returned a message");
            failures++;
        } catch (SofabException e) {
            check("decode() over maxlen", SofabError.INVALID_MSG, e.error());
        }

        // ---- a genuine bug is NOT a refusal ---------------------------------
        // The point of the checked clause: the caller's refusal handler must not
        // also swallow a programming error. Under the old shape both arrived as
        // java.lang.RuntimeException.
        Refuse nil = null;
        try {
            nil.encode();
            System.out.println("FAIL: encoding through a null reference returned bytes");
            failures++;
        } catch (SofabException e) {
            System.out.println("FAIL: an NPE reached the refusal handler as " + e.error());
            failures++;
        } catch (NullPointerException e) {
            // As it should: unchecked, and no refusal handler sees it.
        }

        // ---- the clean path still returns bytes and reads them back ---------
        Refuse ok = new Refuse();
        ok.s = "12345678";
        byte[] wire = ok.encode();
        Refuse back = Refuse.decode(wire);
        if (!ok.s.equals(back.s)) {
            System.out.println("FAIL: round trip lost the value: " + back.s);
            failures++;
        }

        if (failures != 0) {
            System.exit(1);
        }
        System.out.println("refusal type OK");
    }
}
