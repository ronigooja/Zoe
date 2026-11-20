# Writing prompts

Rules for the instruction text in `deploy/prompts/`. The prompt lifecycle
(loading, installation, deployment) is defined in
[configuration](design/flows/configuration.md).

- Every sentence must be able to change the model's output. Remove sentences the
  model cannot act on, choose between, or that do not affect the result.
- State the basis of a decision as the outcome the user wants, not the words the
  user said.
- Define scope by definition, not by example.
- Each rule defines only itself; do not draw a boundary by negating another
  rule.
- Define a concept once. A word appears only where it does work.
- Structure each prompt as input, choice, output content, output format. Put
  each sentence on its own line.
