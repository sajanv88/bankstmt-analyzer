<!--
Placeholder. Replace the entire contents of this file with the real
analysis system prompt.

This file is embedded into the binary at build time (internal/llm) and sent
as the system message of every Azure OpenAI analysis call, so editing it
requires a rebuild, not a redeploy of configuration.

The prompt must instruct the model to return a single JSON object holding
the `analysis`, `savings_plan`, `chart_data.present`, `chart_data.forecast`
and `transactions` keys that internal/llm decodes and internal/pipeline
persists. Changing those top-level key names means changing the Go structs
in internal/llm to match.
-->

You are a financial analyst. Analyse the supplied bank statements and reply
with a single JSON object.
