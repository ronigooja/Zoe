import assert from "node:assert/strict"
import { test } from "node:test"
import { bounded, clean, readResponse, requestText, responseText } from "./opencode_context.mjs"

test("uses the actual request text in order without protocol fields", () => {
  const text = requestText({
    model: "hidden", temperature: 0.7, tools: [{ name: "schema-only" }],
    messages: [
      { role: "system", content: "exact system\n  text" },
      { role: "user", content: [{ type: "text", text: "remember?" }] },
      { role: "assistant", tool_calls: [{ id: "protocol-id", function: { name: "lookup", arguments: '{"q":"a"}' } }] },
      { role: "tool", tool_call_id: "protocol-id", content: "actual result" },
    ],
  })
  assert.equal(text, 'system:\nexact system\n  text\n\nuser:\nremember?\n\nTool call:\nlookup\n{"q":"a"}\n\ntool:\nactual result')
  assert.match(requestText({ previous_response_id: "opaque", input: "next" }), /earlier context.*\n\nuser:\nnext/s)
})

test("decodes real SSE text and tool deltas without duplicating them", async () => {
  const delta = value => `data: ${JSON.stringify({ choices: [{ index: 0, delta: value }] })}\r\n\r\n`
  const stream = delta({ content: "你" }) + delta({ content: "好" }) +
    delta({ tool_calls: [{ index: 0, function: { name: "search", arguments: '{"q"' } }] }) +
    delta({ tool_calls: [{ index: 0, function: { arguments: ':"x"}' } }] }) + "data: [DONE]"
  const bytes = new TextEncoder().encode(stream)
  const response = new Response(new ReadableStream({
    start(controller) {
      for (let i = 0; i < bytes.length; i++) controller.enqueue(bytes.subarray(i, i + 1))
      controller.close()
    },
  }), { headers: { "content-type": "text/event-stream" } })
  assert.deepEqual(await readResponse(response, new AbortController().signal), {
    text: 'assistant:\n你好\n\nTool call:\nsearch\n{"q":"x"}', incomplete: false,
  })
})

test("keeps final Responses output once and marks partial streams", () => {
  const capture = responseText()
  capture.frame(JSON.stringify({ type: "response.output_text.delta", delta: "one" }))
  assert.deepEqual(capture.result(), { text: "assistant:\none", incomplete: true })
  capture.frame(JSON.stringify({ type: "response.completed", response: { output: [
    { role: "assistant", content: [{ type: "output_text", text: "one" }] },
    { type: "function_call", name: "read", arguments: "{}" },
  ] } }))
  assert.deepEqual(capture.result(), { text: "assistant:\none\n\nTool call:\nread\n{}", incomplete: false })
})

test("redacts credential values before UTF-8 truncation", () => {
  const payload = bounded(clean('Bearer token123 {"password":"value123"} known-value ' + "好".repeat(3000), ["known-value"]))
  assert.equal(payload.truncated, true)
  assert.ok(new TextEncoder().encode(payload.text).length <= 8192)
  assert.doesNotMatch(payload.text, /token123|value123|known-value|\uFFFD/)
})
