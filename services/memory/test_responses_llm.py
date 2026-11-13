from __future__ import annotations

import unittest
from types import SimpleNamespace
from unittest.mock import Mock

from .responses_llm import ResponsesLLM, reset_request_timeout, set_request_timeout


class ResponsesAdapterTests(unittest.TestCase):
    def adapter(self, model="gpt-6-luna", effort="low"):
        adapter = object.__new__(ResponsesLLM)
        object.__setattr__(adapter, "config", SimpleNamespace(
            model=model, max_tokens=100, temperature=0.1, top_p=1.0, reasoning_effort=effort
        ))
        client = Mock()
        client.with_options.return_value = client
        client.responses.create.return_value = SimpleNamespace(output_text="result", output=[])
        object.__setattr__(adapter, "client", client)
        return adapter, client

    def test_direct_call_reasoning_and_retries(self):
        for configured, expected in (("high", "high"), ("", "medium")):
            adapter, client = self.adapter(effort=configured)
            self.assertEqual(adapter.generate_response([]), "result")
            client.with_options.assert_called_once_with(max_retries=0)
            request = client.responses.create.call_args.kwargs
            self.assertEqual(request["reasoning"], {"effort": expected})
            self.assertNotIn("temperature", request)
            self.assertNotIn("timeout", request)

    def test_timeout_context_and_nonreasoning_model(self):
        adapter, client = self.adapter(model="gpt-4.1")
        token = set_request_timeout(2.5)
        try:
            adapter.generate_response([], response_format={"type": "json_object"})
        finally:
            reset_request_timeout(token)
        request = client.responses.create.call_args.kwargs
        self.assertEqual(request["timeout"], 2.5)
        self.assertEqual(request["temperature"], 0.1)
        self.assertEqual(request["text"], {"format": {"type": "json_object"}})
        self.assertNotIn("reasoning", request)

    def test_tools_and_schema_are_translated(self):
        adapter, client = self.adapter()
        client.responses.create.return_value = SimpleNamespace(output_text="", output=[
            SimpleNamespace(type="function_call", name="save", arguments='{"text":"fact"}'),
            SimpleNamespace(type="function_call", name="save", arguments="invalid"),
            SimpleNamespace(type="message"),
        ])
        result = adapter.generate_response([], tools=[{"type": "function", "function": {
            "name": "save", "parameters": {"type": "object"}, "strict": True,
        }}], tool_choice={"type": "function", "function": {"name": "save"}}, response_format={
            "type": "json_schema", "json_schema": {"name": "facts", "schema": {"type": "object"}, "strict": True},
        })
        self.assertEqual(result, {"content": "", "tool_calls": [
            {"name": "save", "arguments": {"text": "fact"}}, {"name": "save", "arguments": {}},
        ]})
        request = client.responses.create.call_args.kwargs
        self.assertEqual(request["tool_choice"], {"type": "function", "name": "save"})
        self.assertEqual(request["tools"][0]["name"], "save")
        self.assertEqual(request["text"]["format"]["name"], "facts")
