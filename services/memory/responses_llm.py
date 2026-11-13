from __future__ import annotations

import json
from contextvars import ContextVar, Token
from typing import Any

from mem0.configs.llms.openai import OpenAIConfig
from mem0.llms.openai import OpenAILLM

_request_timeout_seconds: ContextVar[float | None] = ContextVar("zoe_memory_llm_timeout", default=None)


def set_request_timeout(seconds: float) -> Token[float | None]:
    return _request_timeout_seconds.set(seconds)


def reset_request_timeout(token: Token[float | None]) -> None:
    _request_timeout_seconds.reset(token)


class ResponsesLLM(OpenAILLM):
    """Mem0 adapter for deployments exposing OpenAI Responses instead of Chat Completions."""

    @staticmethod
    def _responses_tools(tools: list[dict[str, Any]] | None) -> list[dict[str, Any]] | None:
        if not tools:
            return None
        result = []
        for tool in tools:
            function = tool.get("function", tool)
            converted = {"type": "function", "name": function["name"]}
            for key in ("description", "parameters", "strict"):
                if key in function:
                    converted[key] = function[key]
            result.append(converted)
        return result

    @staticmethod
    def _responses_tool_choice(choice: Any) -> Any:
        if isinstance(choice, dict) and choice.get("type") == "function":
            function = choice.get("function", {})
            return {"type": "function", "name": function.get("name", "")}
        return choice

    @staticmethod
    def _responses_format(response_format: Any) -> dict[str, Any] | None:
        if not response_format:
            return None
        if response_format.get("type") == "json_object":
            return {"format": {"type": "json_object"}}
        if response_format.get("type") == "json_schema":
            schema = response_format.get("json_schema", {})
            return {
                "format": {
                    "type": "json_schema",
                    "name": schema.get("name", "mem0_response"),
                    "schema": schema.get("schema", {}),
                    "strict": bool(schema.get("strict", False)),
                }
            }
        return None

    def generate_response(
        self,
        messages: list[dict[str, str]],
        response_format: Any = None,
        tools: list[dict[str, Any]] | None = None,
        tool_choice: Any = "auto",
        **kwargs: Any,
    ) -> str | dict[str, Any]:
        request: dict[str, Any] = {
            "model": self.config.model,
            "input": messages,
            "max_output_tokens": self.config.max_tokens,
        }
        timeout_seconds = _request_timeout_seconds.get()
        if timeout_seconds is not None:
            request["timeout"] = timeout_seconds
        model_name = str(self.config.model).lower().rsplit("/", 1)[-1]
        reasoning_model = model_name.startswith(("gpt-6", "gpt-5.6", "o1", "o3"))
        if not reasoning_model:
            request["temperature"] = self.config.temperature
            request["top_p"] = self.config.top_p
        if tools:
            request["tools"] = self._responses_tools(tools)
            request["tool_choice"] = self._responses_tool_choice(tool_choice)
        output_format = self._responses_format(response_format)
        if output_format:
            request["text"] = output_format
        reasoning_effort = getattr(self.config, "reasoning_effort", None)
        if reasoning_model:
            request["reasoning"] = {"effort": reasoning_effort or "medium"}
        response = self.client.with_options(max_retries=0).responses.create(**request)
        if not tools:
            return response.output_text or ""
        calls: list[dict[str, Any]] = []
        for item in response.output:
            if getattr(item, "type", "") != "function_call":
                continue
            arguments = getattr(item, "arguments", "{}")
            try:
                parsed = json.loads(arguments)
            except (TypeError, ValueError):
                parsed = {}
            calls.append({"name": item.name, "arguments": parsed})
        return {"content": response.output_text or "", "tool_calls": calls}


def register_responses_provider() -> None:
    # Mem0's provider config validator accepts `openai` but its stock adapter
    # uses Chat Completions. Keep the public config schema and replace only the
    # in-process adapter implementation for this worker.
    from mem0.utils.factory import LlmFactory

    LlmFactory.register_provider(
        "openai", "services.memory.responses_llm.ResponsesLLM", OpenAIConfig
    )
