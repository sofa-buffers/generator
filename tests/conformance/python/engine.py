#!/usr/bin/env python3
"""Which corelib-py engine is loaded IN THIS PROCESS -- the oracle every
per-engine python driver in this directory shares.

run.sh runs each driver once per engine, but its own ``require_engine`` checked a
DIFFERENT process. corelib-py falls back to pure Python whenever ``sofab._speedups``
cannot be imported, and marks the extension ``optional=True``, so an accelerator
that fails to import inside a driver would make the native pass a silent duplicate
of the pure one -- both printing success (generator#451, and the .so-staleness
lesson behind it). The two engines carry independent payload and fp32 paths, which
is the whole reason those legs run twice, so the assertion belongs where the decode
happens.

One module rather than a copy per driver: two copies of the same oracle drift, and
the next per-engine driver would make it three.
"""

import sofab


def require_engine(want):
    """True when the engine loaded here is ``want``; prints why when it is not.

    ``IMPL`` alone is not enough for ``native``: the exported Encoder/Decoder must
    BE the accelerator's, which is what a native leg actually exercises.
    """
    if sofab.IMPL != want:
        print("FAIL: this leg must run on the '%s' engine, but sofab.IMPL is '%s'"
              % (want, sofab.IMPL))
        return False
    if want == "native":
        from sofab import _speedups
        if sofab.Encoder is not _speedups.Encoder or sofab.Decoder is not _speedups.Decoder:
            print("FAIL: sofab.IMPL says 'native' but the exported Encoder/Decoder "
                  "are not the accelerator's")
            return False
    return True
