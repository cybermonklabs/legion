"""Throwaway (scratch branch only): when PROF_LOG is set, logs every subprocess the engine runs -
its start, wall time and CPU - and the interpreter's start and exit, to PROF_LOG as JSON."""
import os

if os.environ.get("PROF_LOG"):
    import atexit, json, subprocess, threading, time

    _t0 = time.monotonic()
    _wall0 = time.time()
    _recs = []
    _lock = threading.Lock()
    _orig_init = subprocess.Popen.__init__

    def _init(self, args, *a, **k):
        self._p_t0 = time.monotonic()
        self._p_args = [str(x) for x in (args if isinstance(args, (list, tuple)) else [args])]
        return _orig_init(self, args, *a, **k)

    def _try_wait(self, flags):
        try:
            pid, sts, ru = os.wait4(self.pid, flags)
        except ChildProcessError:
            return (self.pid, 0)
        if pid and ru is not None:
            with _lock:
                _recs.append({"args": self._p_args[:8], "t0": round(self._p_t0 - _t0, 3),
                              "wall": round(time.monotonic() - self._p_t0, 3),
                              "cpu": round(ru.ru_utime + ru.ru_stime, 3)})
        return (pid, sts)

    subprocess.Popen.__init__ = _init
    subprocess.Popen._try_wait = _try_wait

    def _dump():
        with open(os.environ["PROF_LOG"], "w") as f:
            json.dump({"start_epoch": _wall0, "total": round(time.monotonic() - _t0, 3), "recs": _recs}, f)

    atexit.register(_dump)

    if os.environ.get("PROF_CPROFILE"):
        import cProfile

        _pr = cProfile.Profile()
        _pr.enable()

        def _dump_prof():
            _pr.disable()
            _pr.dump_stats(os.environ["PROF_CPROFILE"])

        atexit.register(_dump_prof)
