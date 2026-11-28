import os
import unittest
from unittest.mock import patch

from services.memory import model_observation as obs


class ObservationTest(unittest.TestCase):
    def test_generation_redaction_and_bounds(self):
        a, b = "a" * 32, "b" * 32
        obs.set_api_token("private-bearer-value")
        obs.drain(a)
        token = obs.set_component("memory_write")
        try:
            with patch.dict(os.environ, {"ZOE_MEM0_LLM_API_KEY": "secret-value"}):
                call = obs.begin([{"role": "user", "content": "secret-value private-bearer-value Bearer abcdefghijklmnop"}], "model")
                obs.finish(call, "secret-value")
            events, overflow = obs.drain(a)
            self.assertFalse(overflow)
            self.assertEqual([e["type"] for e in events], ["request", "response"])
            self.assertTrue(events[0]["conversation_start"])
            self.assertNotIn("conversation_start", events[1])
            self.assertNotIn("secret-value", str(events))
            self.assertNotIn("private-bearer-value", str(events))
            self.assertNotIn("abcdefghijklmnop", str(events))
            old = obs.begin([{"role": "user", "content": "old"}], "model")
            self.assertEqual(obs.drain(b), ([], False))
            obs.finish(old, "old response")
            self.assertEqual(obs.drain(b), ([], False))
            obs.begin([{"role": "user", "content": "x" * 9000}], "model")
            self.assertTrue(obs.drain(b)[0][0]["truncated"])
            for _ in range(70):
                obs.begin([], "model")
            events, overflow = obs.drain(b)
            self.assertEqual(len(events), obs.MAX_EVENTS)
            self.assertTrue(overflow)
            self.assertEqual(obs.drain(b), ([], False))
        finally:
            obs.reset_component(token)

    def test_only_initial_turn_is_known_fresh(self):
        lease = "d" * 32
        obs.drain(lease)
        token = obs.set_component("memory_ingestion")
        try:
            obs.begin([{"role": "system", "content": "instructions"}, {"role": "user", "content": "x"}], "model")
            obs.begin([{"role": "assistant", "content": "earlier"}, {"role": "user", "content": "x"}], "model")
            obs.begin([{"role": "tool", "content": "earlier"}, {"role": "user", "content": "x"}], "model")
            fresh, continuation, unsupported = obs.drain(lease)[0]
            self.assertTrue(fresh["conversation_start"])
            self.assertNotIn("conversation_start", continuation)
            self.assertNotIn("conversation_start", unsupported)
            self.assertTrue(unsupported["incomplete"])
            self.assertFalse(unsupported["truncated"])
        finally:
            obs.reset_component(token)

    def test_expired_call_cannot_cross_lease(self):
        a = "c" * 32
        with patch.object(obs.time, "monotonic") as clock:
            clock.return_value = 1
            obs.drain(a)
            token = obs.set_component("memory_ingestion")
            try:
                call = obs.begin([], "model")
                clock.return_value = 12
                self.assertEqual(obs.drain(a), ([], False))
                obs.finish(call, "late")
                self.assertEqual(obs.drain(a), ([], False))
            finally:
                obs.reset_component(token)
