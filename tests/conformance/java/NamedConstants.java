// Not generated: dropped into the generated example project by the conformance
// harness, which builds it into the same jar as the message classes.
//
// An enum or bitfield field is a `long`, and the names the schema gives its
// values are constants BESIDE it, in one holder class per type (MESSAGE_SPEC §1:
// a target whose native enum cannot hold an undeclared value materializes the
// field as the integer with named constants next to it). This is the user's side
// of that contract, compiled under -Xlint:all -Werror:
//
//   * the holders exist under the names the field javadoc links, and their
//     constants are compile-time `long`s with the schema's values -- a `switch`
//     on them only compiles if they are constant expressions;
//   * a named value assigned through its constant round-trips;
//   * an in-width value that names NO constant still decodes, is stored as
//     received and re-encodes byte-identically -- the reason the field is not a
//     Java enum -- and a bitfield bit no flag declares is kept, never masked.
package message;

import java.util.Arrays;

public final class NamedConstants {

    private static int failures = 0;

    private static void check(boolean ok, String what) {
        if (!ok) {
            System.out.println("FAIL: " + what);
            failures++;
        }
    }

    /** Only compiles while every case label is a constant expression. */
    private static String colour(long v) {
        switch ((int) v) {
            case (int) Myfirstmessage_Someenum.RED:
                return "red";
            case (int) Myfirstmessage_Someenum.GREEN:
                return "green";
            case (int) Myfirstmessage_Someenum.BLUE:
                return "blue";
            case (int) Myfirstmessage_Someenum.YELLOW:
                return "yellow";
            default:
                return null;
        }
    }

    public static void main(String[] args) throws Exception {
        check(Myfirstmessage_Someenum.RED == 0L, "RED == 0");
        check(Myfirstmessage_Someenum.GREEN == -100L, "GREEN == -100");
        check(Myfirstmessage_Someenum.BLUE == 2L, "BLUE == 2");
        check(Myfirstmessage_Someenum.YELLOW == 33L, "YELLOW == 33");
        check(Myfirstmessage_Somebitfield.flagA == 1L, "flagA is the mask of bit 0");
        check(Myfirstmessage_Somebitfield.flagB == 2L, "flagB is the mask of bit 1");
        check(Myfirstmessage_Someenumarray.BLUE == 2L, "the array element's enum BLUE == 2");

        // The field's default is the schema's, spelled by its constant.
        Myfirstmessage fresh = new Myfirstmessage();
        check(fresh.someenum == Myfirstmessage_Someenum.BLUE, "someenum defaults to BLUE");
        check((fresh.somebitfield & Myfirstmessage_Somebitfield.flagB) != 0, "flagB defaults to set");
        check((fresh.somebitfield & Myfirstmessage_Somebitfield.flagA) == 0, "flagA defaults to clear");

        // A named value, through its constant.
        Myfirstmessage m = new Myfirstmessage();
        m.someenum = Myfirstmessage_Someenum.GREEN;
        m.somebitfield = Myfirstmessage_Somebitfield.flagA | Myfirstmessage_Somebitfield.flagB;
        Myfirstmessage back = Myfirstmessage.decode(m.encode());
        check(back.someenum == Myfirstmessage_Someenum.GREEN, "GREEN round-trips");
        check("green".equals(colour(back.someenum)), "GREEN reads back as green");
        check(back.somebitfield == (Myfirstmessage_Somebitfield.flagA | Myfirstmessage_Somebitfield.flagB),
                "flagA|flagB round-trips");

        // An in-width value that names no constant, and a bit no flag declares.
        Myfirstmessage u = new Myfirstmessage();
        u.someenum = 7L;
        u.somebitfield = Myfirstmessage_Somebitfield.flagA | (1L << 5);
        byte[] wire = u.encode();
        Myfirstmessage ub = Myfirstmessage.decode(wire);
        check(ub.someenum == 7L, "an unnamed enum value is stored as received");
        check(colour(ub.someenum) == null, "7 names no constant");
        check(ub.somebitfield == (Myfirstmessage_Somebitfield.flagA | (1L << 5)),
                "an undeclared bit is kept, never masked");
        check(Arrays.equals(ub.encode(), wire), "the unnamed values re-encode byte-identically");

        if (failures != 0) {
            System.exit(1);
        }
        System.out.println("named constants OK");
    }
}
