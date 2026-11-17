"""Regression tests for the MicroPython prelude's repeating callbacks.

The prelude (`sdk.py`) is embedded in the host and cannot import a real bus in a
unit test, so this loads it unchanged against a stub ``capsulad`` module that
models the C primitives it uses. It covers the timer surface added for
autonomous app watchdogs: the cap, run-loop scheduling with no events, the poll
bound, cancel-during-pass, callback isolation and shutdown.

Run with: python3 interpreter/micropython/sdk_test.py
"""

import importlib.util
import os
import sys
import unittest


class StubHost:
    """The C module surface the prelude programs against."""

    def __init__(self):
        self.now = 0
        self.polls = []
        self.logs = []
        self.stopped = False
        # Note: the prelude publishes its own `disconnected` wrapper on this
        # module object, so the flag must not use that name.
        self.disconnected_flag = False
        self.polls_left = 6
        self.clock_error = False

    # --- primitives used by the prelude ------------------------------------

    def _env(self):
        return {"appId": "test.app", "runId": "run_test", "sdk": "0.5",
                "socket": "", "library": "", "appDir": ""}

    def _monotonic_ms(self):
        if self.clock_error:
            raise OSError(5, "clock unavailable")
        return self.now

    def _log(self, message):
        self.logs.append(message)

    def _start(self, timeout):
        pass

    def _stopped(self):
        return self.stopped

    def _disconnected(self):
        return self.disconnected_flag

    def _poll(self, timeout):
        """Model the real poll: block up to `timeout` ms, then report idle."""
        self.polls.append(timeout)
        self.now += timeout
        self.polls_left -= 1
        if self.polls_left <= 0:
            self.stopped = True
        return None

    def _release(self, handle):
        pass

    def _disconnect_status(self):
        return 0

    def _disconnect_detail(self):
        return ""

    def _ready(self):
        return True

    def _connected(self):
        return True


def load_prelude(stub):
    sys.modules["capsulad"] = stub
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sdk.py")
    spec = importlib.util.spec_from_file_location("capsulad_prelude", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class PreludeTimerTest(unittest.TestCase):
    def setUp(self):
        self.stub = StubHost()
        self.prelude = load_prelude(self.stub)

    def run_loop(self, polls=6):
        """Drive the real run loop for `polls` polls.

        The poll that reports the stop ends the loop before that iteration's
        timer pass, so `polls` polls run timers `polls - 1` times.
        """
        self.stub.polls_left = polls
        self.stub.stopped = False
        self.stub.start()
        self.stub.run()

    # --- validation and bounds --------------------------------------------

    def test_interval_must_be_positive(self):
        with self.assertRaises(ValueError):
            self.stub.every(0, lambda: None)
        with self.assertRaises(ValueError):
            self.stub.every(-5, lambda: None)
        with self.assertRaises(TypeError):
            self.stub.every(100, "not callable")

    def test_callback_count_is_capped(self):
        for _ in range(16):
            self.stub.every(1000, lambda: None)
        with self.assertRaises(ValueError):
            self.stub.every(1000, lambda: None)
        self.stub.cancel_timer(1)
        self.stub.every(1000, lambda: None)   # room again

    def test_cancel_timer_reports_whether_it_existed(self):
        token = self.stub.every(1000, lambda: None)
        self.assertTrue(self.stub.cancel_timer(token))
        self.assertFalse(self.stub.cancel_timer(token))

    # --- scheduling --------------------------------------------------------

    def test_callbacks_fire_with_no_events_at_all(self):
        fired = []
        self.stub.every(100, lambda: fired.append(self.stub.now))
        self.run_loop(polls=7)
        self.assertEqual(len(fired), 6)
        self.assertEqual(fired[0], 100)
        self.assertEqual(fired[-1], 600)

    def test_poll_is_bounded_by_the_nearest_timer(self):
        self.stub.every(250, lambda: None)
        self.run_loop(polls=4)
        self.assertEqual(self.stub.polls, [250, 250, 250, 250])

    def test_poll_falls_back_to_one_second_without_timers(self):
        self.run_loop(polls=3)
        self.assertEqual(self.stub.polls, [1000, 1000, 1000])

    def test_slow_callback_slips_instead_of_piling_up(self):
        calls = []

        def slow():
            calls.append(self.stub.now)
            self.stub.now += 400   # the callback itself takes 400 ms

        self.stub.every(100, slow)
        self.run_loop(polls=6)
        # At most one run per loop pass, and the next due time is measured from
        # completion, so a slow callback can never be scheduled back-to-back.
        self.assertEqual(len(calls), 5)
        for index in range(1, len(calls)):
            self.assertGreaterEqual(calls[index] - calls[index - 1], 400)

    # --- isolation and lifecycle ------------------------------------------

    def test_a_failing_callback_is_logged_and_keeps_its_schedule(self):
        calls = []

        def boom():
            calls.append(1)
            raise RuntimeError("boom")

        self.stub.every(100, boom)
        self.run_loop(polls=5)
        self.assertEqual(len(calls), 4)
        self.assertEqual(len([line for line in self.stub.logs
                              if line.startswith("timer handler failed")]), 4)

    def test_cancel_during_a_pass_stops_the_cancelled_callback(self):
        fired = []
        state = {}

        def first():
            fired.append("first")
            self.stub.cancel_timer(state["second"])

        def second():
            fired.append("second")

        self.stub.every(100, first)
        state["second"] = self.stub.every(100, second)
        self.run_loop(polls=2)
        self.assertEqual(fired, ["first"])

    def test_callbacks_stop_when_the_loop_ends(self):
        fired = []
        self.stub.every(100, lambda: fired.append(1))
        self.run_loop(polls=3)
        self.assertEqual(len(fired), 2)
        self.assertEqual(self.prelude._timers, [])
        self.stub.now += 1000
        self.prelude._run_timers()
        self.assertEqual(len(fired), 2)

    def test_a_broken_clock_does_not_end_the_run(self):
        self.stub.every(100, lambda: None)
        self.stub.clock_error = True
        self.assertEqual(self.prelude._poll_timeout(), 1000)
        self.prelude._run_timers()          # must not raise
        self.run_loop(polls=2)              # loop survives

    def test_registering_from_a_callback_is_bounded(self):
        state = {"sizes": []}

        def grow():
            state["sizes"].append(len(self.prelude._timers))
            try:
                self.stub.every(1000, lambda: None)
            except ValueError as err:
                state["error"] = str(err)

        self.stub.every(100, grow)
        self.run_loop(polls=40)
        self.assertIn("at most 16 repeating callbacks", state.get("error", ""))
        self.assertEqual(max(state["sizes"]), 16)


if __name__ == "__main__":
    unittest.main(verbosity=2)
