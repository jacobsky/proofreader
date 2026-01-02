# Retry Prompt

You must respond with VALID JSON only. Your previous response was not valid JSON.
Reformat this response as proper JSON:
{LLM_RESPONSE}
Return ONLY a JSON object with this exact structure:
{
"original_sentence": "string",
"corrected_sentence": "string",
"error_details": "string",
"reason": "string",
"misused_words": []
}