# converse-tui

A small terminal chat client for the [Amazon Bedrock Converse API](https://docs.aws.amazon.com/bedrock/latest/userguide/conversation-inference.html), written in Go. It streams responses, keeps conversation history, and runs on Linux (including RHEL) and Windows.

## Install

**From source** (needs Go 1.24+):

```sh
git clone https://github.com/aachtenberg/converse-tui.git
cd converse-tui
go build -o converse-tui .        # Windows: go build -o converse-tui.exe .
```

**Prebuilt binaries:** see [Releases](https://github.com/aachtenberg/converse-tui/releases) (linux/amd64, linux/arm64, windows/amd64). These are built by GitHub Actions when a `v*` tag is pushed.

## Configure

1. **AWS credentials:** use any normal AWS method: `aws configure`, `aws sso login --profile <name>`, environment variables, or an instance role. converse-tui never stores credentials.
2. **Create a config file:**

   ```sh
   converse-tui -init
   ```

   This writes a commented example to:
   - Linux: `~/.config/converse-tui/config.yaml`
   - Windows: `%AppData%\converse-tui\config.yaml`

3. **Set `model_id`** to a model or inference profile that your account can access:

   ```sh
   aws bedrock list-inference-profiles --region us-east-1 \
     --query "inferenceProfileSummaries[].inferenceProfileId"
   ```

### Settings

| YAML key        | Env var                      | Flag       | Notes |
|-----------------|------------------------------|------------|-------|
| `profile`       | `CONVERSE_TUI_PROFILE`       | `-profile` | AWS profile; empty = `AWS_PROFILE` / default |
| `region`        | `CONVERSE_TUI_REGION`        | `-region`  | empty = profile region / `AWS_REGION` |
| `model_id`      | `CONVERSE_TUI_MODEL_ID`      | `-model`   | **required** |
| `system_prompt` | `CONVERSE_TUI_SYSTEM_PROMPT` |            | optional |
| `max_tokens`    | `CONVERSE_TUI_MAX_TOKENS`    |            | 0 = model default |
| `temperature`   | `CONVERSE_TUI_TEMPERATURE`   |            | only sent if set (newer Claude models reject it) |
| `tools`         | `CONVERSE_TUI_TOOLS`         | `-no-tools`| file tools on/off (default on) |
| `workdir`       | `CONVERSE_TUI_WORKDIR`       | `-workdir` | directory tools work in; empty = current directory |
| `auto_approve`  | `CONVERSE_TUI_AUTO_APPROVE`  | `-auto`    | apply edits without asking (toggle with Ctrl+T) |
| *(file path)*   | `CONVERSE_TUI_CONFIG`        | `-config`  | alternate config file |

Precedence: config file < environment variables < flags. You can also skip the file and use only env vars or flags:

```sh
converse-tui -profile work -region us-east-1 -model <inference-profile-id>
```

```powershell
$env:CONVERSE_TUI_MODEL_ID = "<inference-profile-id>"; .\converse-tui.exe
```

## File tools

The model can use `list_dir`, `read_file`, `write_file` and `edit_file` (exact find-and-replace).

| Where the path is | Read / list | Write / edit |
|---|---|---|
| Inside the working directory, any depth (`./here`, `./here/there/somewhere`) | runs automatically | asks **y/n** with a preview, unless auto mode is on |
| Outside it (`../x`, absolute paths, symlinks that point out) | **always asks** | **always asks**, even in auto mode |

At an edit prompt: `y` apply, `n` deny (the model is told), `a` apply and turn on auto mode, `Esc` cancel the turn. The header shows **AUTO** while auto mode is on.

## Keys

| Key                     | Action |
|-------------------------|--------|
| Enter                   | Send |
| Alt+Enter / Ctrl+J      | New line |
| Esc                     | Cancel the response in progress |
| Ctrl+N                  | New conversation |
| Ctrl+T                  | Toggle auto-approve for edits |
| PgUp / PgDn, Ctrl+U / D | Scroll |
| Ctrl+C                  | Quit |

## Corporate networks

The AWS SDK reads the standard settings:

- Proxy: `HTTPS_PROXY` / `NO_PROXY`
- Custom CA bundle (TLS-inspecting proxies): `AWS_CA_BUNDLE=/path/to/ca.pem`, or `ca_bundle` in your AWS profile
- If `go build` can't reach `proxy.golang.org`, set `GOPROXY` to your internal mirror, or download a release binary instead.

On Windows, use Windows Terminal or a recent PowerShell console so colors and the alternate screen display correctly.

## IAM permissions

The caller needs `bedrock:InvokeModelWithResponseStream` on the model or inference profile (plus the underlying foundation models when using a cross-region inference profile).

## License

MIT
