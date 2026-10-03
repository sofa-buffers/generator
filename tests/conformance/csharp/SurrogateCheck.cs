// Not generated: dropped into a generated project by the conformance harness,
// REPLACING the generated Program.cs, the way OwnershipCheck.cs does.
//
// An unpaired UTF-16 surrogate in a string is not text (CORELIB_PLAN §6.4.1): the
// encoder must refuse it as InvalidArgument and write nothing, never replace it
// with U+FFFD. The string is built here with (char)0xD800 and handed straight to
// the generated message's Encode(). The JSON harness cannot carry this case:
// System.Text.Json refuses a lone-surrogate escape itself, before any corelib
// call, so a row driven through it passes whatever the corelib does.
//
// Positions mirror tests/conformance/lib/check_utf8_positions.py: the top-level
// string with four surrogate shapes, then a lone high surrogate at every other
// string position of examples/messages/example.yaml.
using System;
using System.Collections.Generic;
using sofab;
using Sofabuffers;

internal static class SurrogateCheck {
    private static int failures = 0;
    private static int rows = 0;

    private static void MustRefuse(string what, Action<Myfirstmessage, string> place, string text) {
        rows++;
        var m = new Myfirstmessage();
        place(m, text);
        try {
            var bytes = m.Encode();
            Console.WriteLine($"FAIL: {what}: encoded {bytes.Length} bytes instead of refusing");
            failures++;
        } catch (SofabException e) {
            if (e.Error != SofabError.Argument) {
                Console.WriteLine($"FAIL: {what}: refused with {e.Error}, want Argument");
                failures++;
            }
        } catch (Exception e) {
            Console.WriteLine($"FAIL: {what}: threw {e.GetType().Name}, not SofabException: {e.Message}");
            failures++;
        }
    }

    private static int Main() {
        var hi = "a" + (char)0xD800 + "b";
        var shapes = new (string, string)[] {
            ("lone_high", hi),
            ("lone_low", "a" + (char)0xDC00 + "b"),
            ("trailing_high", "ab" + (char)0xD800),
            ("reversed_pair", "a" + (char)0xDC00 + (char)0xD800 + "b"),
        };
        foreach (var (n, t) in shapes) {
            MustRefuse("somestring/" + n, (m, s) => m.somestring = s, t);
        }
        MustRefuse("somestringarray[0]/lone_high",
            (m, s) => m.somestringarray = new List<string>{ s }, hi);
        MustRefuse("somestringarray[2]/lone_high",
            (m, s) => m.somestringarray = new List<string>{ "a", "b", s }, hi);
        MustRefuse("somestruct.nestedstring/lone_high",
            (m, s) => m.somestruct.nestedstring = s, hi);
        MustRefuse("somestructwitharray.label/lone_high",
            (m, s) => m.somestructwitharray.label = s, hi);
        MustRefuse("someunion.option2/lone_high",
            (m, s) => m.someunion.Option2 = s, hi);
        MustRefuse("someunionarray[1].asstring/lone_high",
            (m, s) => m.someunionarray = new List<Myfirstmessage_Someunionarray>{
                new Myfirstmessage_Someunionarray{ Asint = 1 },
                new Myfirstmessage_Someunionarray{ Asstring = s } }, hi);
        MustRefuse("somemap[1].key/lone_high",
            (m, s) => m.somemap = new List<Myfirstmessage_Somemap>{
                new Myfirstmessage_Somemap{ key = "a", value = 1 },
                new Myfirstmessage_Somemap{ key = s, value = 2 } }, hi);

        if (failures != 0) { return 1; }
        Console.WriteLine($"   [csharp] {rows} unpaired-surrogate refusals (InvalidArgument)");
        return 0;
    }
}
