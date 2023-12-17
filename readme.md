# speecher

A discord bot that glues together ExtractorAPI and OpenAI TTS.

It registers the `/speak` command with discord, which takes a `url` and optional `voice`. It then uses `go-readability` to extract the text from the url, and sends it to OpenAI TTS to generate an audio file. The audio file is then uploaded to S3, and the link is sent back to the user.

Env vars:

```
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
AWS_REGION
DISCORD_TOKEN
OPENAI_API_KEY
S3_BUCKET
```

## External Services

- AWS account with S3 bucket, and IAM user with access to that bucket
- Discord bot account https://discord.com/developers/applications
- OpenAI account https://platform.openai.com/