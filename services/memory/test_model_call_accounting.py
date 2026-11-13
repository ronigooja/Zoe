from __future__ import annotations

import unittest
from types import SimpleNamespace
from typing import Any

from .responses_llm import ResponsesLLM, reset_model_call_context, set_model_call_context, unaccounted_sync_call


class _ControlAPI:
    def __init__(self, dispatch_allowed: bool = True):
        self.dispatch_allowed = dispatch_allowed
        self.events: list[tuple[str, dict[str, Any]]] = []

    def reserve_model_call(self, *args: Any) -> dict[str, Any]:
        self.events.append(("reserve", {"args": args}))
        return {"call_id": "call-1", "dispatch_allowed": self.dispatch_allowed}

    def finalize_model_call(self, *args: Any, **kwargs: Any) -> dict[str, Any]:
        self.events.append(("finalize", {"args": args, "kwargs": kwargs}))
        return {"call_id": "call-1", "status": "finalized"}


class _ResponsesClient:
    def __init__(self, response: Any = None, error: Exception | None = None):
        self.response = response
        self.error = error
        self.retry_values: list[int] = []
        self.calls = 0
        self.responses = SimpleNamespace(create=self.create)

    def with_options(self, *, max_retries: int) -> _ResponsesClient:
        self.retry_values.append(max_retries)
        return self

    def create(self, **kwargs: Any) -> Any:
        self.last_request = kwargs
        self.calls += 1
        if self.error is not None:
            raise self.error
        return self.response


def _adapter(client: _ResponsesClient) -> ResponsesLLM:
    adapter = object.__new__(ResponsesLLM)
    object.__setattr__(adapter, "config", SimpleNamespace(
        model="gpt-6-luna", max_tokens=100, temperature=0.1, top_p=1.0, reasoning_effort="low"
    ))
    object.__setattr__(adapter, "client", client)
    return adapter


class ModelCallAccountingTests(unittest.TestCase):
    def test_missing_job_accounting_context_refuses_dispatch(self) -> None:
        client = _ResponsesClient()
        adapter = _adapter(client)
        with self.assertRaisesRegex(RuntimeError, "unaccounted dispatch"):
            adapter.generate_response([{"role": "user", "content": "synthetic"}])
        self.assertEqual(client.calls, 0)

    def test_configured_reasoning_effort_reaches_request_and_empty_keeps_medium(self) -> None:
        for configured, expected in (("high", "high"), ("", "medium")):
            api = _ControlAPI()
            client = _ResponsesClient(SimpleNamespace(id="resp", output_text="", output=[], usage=SimpleNamespace(input_tokens=1, output_tokens=1)))
            adapter = _adapter(client)
            adapter.config.reasoning_effort = configured
            token = set_model_call_context(api, {"id": "job-1", "attempt": 1})
            try:
                adapter.generate_response([{"role": "user", "content": "synthetic"}])
            finally:
                reset_model_call_context(token)
            request = getattr(client, "last_request", None)
            self.assertEqual(request["reasoning"]["effort"], expected)

    def test_sync_opt_out_resets_after_success_and_exception(self) -> None:
        client = _ResponsesClient(SimpleNamespace(output_text="synthetic"))
        adapter = _adapter(client)
        with unaccounted_sync_call():
            self.assertEqual(adapter.generate_response([]), "synthetic")
        with self.assertRaisesRegex(RuntimeError, "unaccounted dispatch"):
            adapter.generate_response([])
        with self.assertRaisesRegex(ValueError, "synthetic"):
            with unaccounted_sync_call():
                raise ValueError("synthetic")
        with self.assertRaisesRegex(RuntimeError, "unaccounted dispatch"):
            adapter.generate_response([])
        self.assertEqual(client.calls, 1)

    def test_sync_opt_out_does_not_override_existing_job_budget_gate(self) -> None:
        api = _ControlAPI(dispatch_allowed=False)
        client = _ResponsesClient()
        adapter = _adapter(client)
        token = set_model_call_context(api, {"id": "job-1", "attempt": 1})
        try:
            with unaccounted_sync_call():
                with self.assertRaisesRegex(RuntimeError, "refusing duplicate dispatch"):
                    adapter.generate_response([])
        finally:
            reset_model_call_context(token)
        self.assertEqual(client.calls, 0)
        self.assertEqual([event[0] for event in api.events], ["reserve"])

    def test_reserves_before_one_physical_call_and_records_provider_usage(self) -> None:
        api = _ControlAPI()
        client = _ResponsesClient(SimpleNamespace(
            id="resp_safe-id", output_text="", output=[], usage=SimpleNamespace(input_tokens=31, output_tokens=7)
        ))
        adapter = _adapter(client)
        token = set_model_call_context(api, {"id": "job-1", "attempt": 1})
        try:
            adapter.generate_response([{"role": "user", "content": "这是合成测试文字"}])
        finally:
            reset_model_call_context(token)

        self.assertEqual([event[0] for event in api.events], ["reserve", "finalize"])
        reserve = api.events[0][1]["args"]
        self.assertEqual(reserve[:2], ("job-1", 1))
        self.assertEqual(reserve[3:5], ("openai_responses", "gpt-6-luna"))
        self.assertGreater(reserve[5], len("这是合成测试文字"))
        self.assertEqual(client.retry_values, [0])
        self.assertEqual(client.calls, 1)
        finalize = api.events[1][1]
        self.assertEqual(finalize["args"][2:5], (1, "succeeded", "measured"))
        self.assertEqual(finalize["args"][5:7], (31, 7))
        self.assertEqual(finalize["args"][7], "resp_safe-id")

    def test_replayed_reservation_refuses_provider_dispatch(self) -> None:
        api = _ControlAPI(dispatch_allowed=False)
        client = _ResponsesClient()
        adapter = _adapter(client)
        token = set_model_call_context(api, {"id": "job-1", "attempt": 1})
        try:
            with self.assertRaisesRegex(RuntimeError, "refusing duplicate dispatch"):
                adapter.generate_response([{"role": "user", "content": "synthetic"}])
        finally:
            reset_model_call_context(token)
        self.assertEqual(client.calls, 0)
        self.assertEqual(api.events, [("reserve", api.events[0][1])])

    def test_provider_failure_is_finalized_with_unknown_usage(self) -> None:
        api = _ControlAPI()
        client = _ResponsesClient(error=TimeoutError("synthetic timeout"))
        adapter = _adapter(client)
        token = set_model_call_context(api, {"id": "job-1", "attempt": 1})
        try:
            with self.assertRaises(TimeoutError):
                adapter.generate_response([{"role": "user", "content": "synthetic"}])
        finally:
            reset_model_call_context(token)
        self.assertEqual([event[0] for event in api.events], ["reserve", "finalize"])
        finalize = api.events[1][1]
        self.assertEqual(finalize["args"][2:5], (1, "failed", "unknown"))
        self.assertEqual(finalize["args"][5:], ())


if __name__ == "__main__":
    unittest.main()
