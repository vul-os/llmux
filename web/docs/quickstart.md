# Quickstart

llmux is a single Go binary that speaks the **OpenAI-compatible HTTP API** and
routes to any provider behind it. Run it, point any OpenAI client at it, done.

## Run the gateway

```bash
# build the binary (or grab a release)
make build

# providers are auto-detected from environment variables
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...

./dist/llmux            # listening on http://localhost:4000
```

## Call it from any language

llmux speaks the OpenAI wire protocol, so **any OpenAI SDK works** — point its
base URL at llmux and use your virtual key. The `model` string selects the route
(an alias, a `provider/model`, or a strategy like `"cheapest"`). For languages
without an SDK it's a plain HTTP `POST` to `/v1/chat/completions`. Add
`"stream": true` for byte-identical OpenAI SSE.

**curl**

```bash
curl http://localhost:4000/v1/chat/completions \
  -H "Authorization: Bearer sk-team-a" \
  -H "Content-Type: application/json" \
  -d '{"model":"cheapest","messages":[{"role":"user","content":"hi"}]}'
```

**Python** — `pip install openai`

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:4000/v1", api_key="sk-team-a")
res = client.chat.completions.create(
    model="cheapest",                       # any provider, one client
    messages=[{"role": "user", "content": "hi"}],
)
print(res.choices[0].message.content)
```

**Node.js / JavaScript** — `npm install openai`

```javascript
import OpenAI from "openai";

const client = new OpenAI({ baseURL: "http://localhost:4000/v1", apiKey: "sk-team-a" });
const res = await client.chat.completions.create({
  model: "cheapest",
  messages: [{ role: "user", content: "hi" }],
});
console.log(res.choices[0].message.content);
```

**TypeScript** — same SDK, fully typed

```typescript
import OpenAI from "openai";

const client = new OpenAI({ baseURL: "http://localhost:4000/v1", apiKey: "sk-team-a" });
const res: OpenAI.Chat.ChatCompletion = await client.chat.completions.create({
  model: "anthropic/claude-3-5-sonnet",
  messages: [{ role: "user", content: "hi" }],
});
console.log(res.choices[0]?.message.content);
```

**Go** — `go get github.com/sashabaranov/go-openai`

```go
cfg := openai.DefaultConfig("sk-team-a")
cfg.BaseURL = "http://localhost:4000/v1"
client := openai.NewClientWithConfig(cfg)

res, _ := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
    Model:    "cheapest",
    Messages: []openai.ChatCompletionMessage{{Role: "user", Content: "hi"}},
})
fmt.Println(res.Choices[0].Message.Content)
```

**Ruby** — `gem install ruby-openai`

```ruby
require "openai"

client = OpenAI::Client.new(access_token: "sk-team-a", uri_base: "http://localhost:4000/v1")
res = client.chat(parameters: {
  model: "cheapest",
  messages: [{ role: "user", content: "hi" }],
})
puts res.dig("choices", 0, "message", "content")
```

**PHP** — `composer require openai-php/client`

```php
$client = OpenAI::factory()
    ->withApiKey('sk-team-a')
    ->withBaseUri('http://localhost:4000/v1')
    ->make();

$res = $client->chat()->create([
    'model'    => 'cheapest',
    'messages' => [['role' => 'user', 'content' => 'hi']],
]);
echo $res->choices[0]->message->content;
```

**Java** — `com.openai:openai-java`

```java
OpenAIClient client = OpenAIOkHttpClient.builder()
    .apiKey("sk-team-a")
    .baseUrl("http://localhost:4000/v1")
    .build();

ChatCompletionCreateParams params = ChatCompletionCreateParams.builder()
    .model("cheapest")
    .addUserMessage("hi")
    .build();

System.out.println(client.chat().completions().create(params)
    .choices().get(0).message().content().orElse(""));
```

**C#** — `dotnet add package OpenAI`

```csharp
using OpenAI.Chat;
using System.ClientModel;

ChatClient client = new(
    model: "cheapest",
    credential: new ApiKeyCredential("sk-team-a"),
    options: new OpenAI.OpenAIClientOptions { Endpoint = new Uri("http://localhost:4000/v1") });

ChatCompletion res = client.CompleteChat("hi");
Console.WriteLine(res.Content[0].Text);
```

**Rust** — `cargo add async-openai`

```rust
use async_openai::{config::OpenAIConfig, types::*, Client};

let config = OpenAIConfig::new()
    .with_api_base("http://localhost:4000/v1")
    .with_api_key("sk-team-a");
let client = Client::with_config(config);

let req = CreateChatCompletionRequestArgs::default()
    .model("cheapest")
    .messages([ChatCompletionRequestUserMessageArgs::default()
        .content("hi").build()?.into()])
    .build()?;
let res = client.chat().create(req).await?;
println!("{}", res.choices[0].message.content.clone().unwrap_or_default());
```

**C++** — libcurl + [nlohmann/json](https://github.com/nlohmann/json)

```cpp
#include <curl/curl.h>
#include <nlohmann/json.hpp>
#include <string>

static size_t sink(char* p, size_t s, size_t n, void* out) {
  static_cast<std::string*>(out)->append(p, s * n);
  return s * n;
}

int main() {
  nlohmann::json body = {
    {"model", "cheapest"},
    {"messages", {{{"role", "user"}, {"content", "hi"}}}},
  };
  std::string payload = body.dump(), resp;

  CURL* c = curl_easy_init();
  curl_slist* h = nullptr;
  h = curl_slist_append(h, "Content-Type: application/json");
  h = curl_slist_append(h, "Authorization: Bearer sk-team-a");
  curl_easy_setopt(c, CURLOPT_URL, "http://localhost:4000/v1/chat/completions");
  curl_easy_setopt(c, CURLOPT_HTTPHEADER, h);
  curl_easy_setopt(c, CURLOPT_POSTFIELDS, payload.c_str());
  curl_easy_setopt(c, CURLOPT_WRITEFUNCTION, sink);
  curl_easy_setopt(c, CURLOPT_WRITEDATA, &resp);
  curl_easy_perform(c);

  auto j = nlohmann::json::parse(resp);
  printf("%s\n", j["choices"][0]["message"]["content"].get<std::string>().c_str());
  curl_slist_free_all(h);
  curl_easy_cleanup(c);
}
```

**C** — libcurl (the JSON response prints to stdout)

```c
#include <curl/curl.h>

int main(void) {
  const char *body =
    "{\"model\":\"cheapest\","
    "\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}";

  CURL *c = curl_easy_init();
  struct curl_slist *h = NULL;
  h = curl_slist_append(h, "Content-Type: application/json");
  h = curl_slist_append(h, "Authorization: Bearer sk-team-a");
  curl_easy_setopt(c, CURLOPT_URL, "http://localhost:4000/v1/chat/completions");
  curl_easy_setopt(c, CURLOPT_HTTPHEADER, h);
  curl_easy_setopt(c, CURLOPT_POSTFIELDS, body);
  curl_easy_perform(c);

  curl_slist_free_all(h);
  curl_easy_cleanup(c);
  return 0;
}
```

**Swift** — URLSession (async/await)

```swift
var req = URLRequest(url: URL(string: "http://localhost:4000/v1/chat/completions")!)
req.httpMethod = "POST"
req.setValue("Bearer sk-team-a", forHTTPHeaderField: "Authorization")
req.setValue("application/json", forHTTPHeaderField: "Content-Type")
req.httpBody = try JSONSerialization.data(withJSONObject: [
  "model": "cheapest",
  "messages": [["role": "user", "content": "hi"]],
])

let (data, _) = try await URLSession.shared.data(for: req)
print(String(decoding: data, as: UTF8.self))
```

**Kotlin** — `java.net.http`

```kotlin
import java.net.URI
import java.net.http.*

val body = """{"model":"cheapest","messages":[{"role":"user","content":"hi"}]}"""
val req = HttpRequest.newBuilder(URI("http://localhost:4000/v1/chat/completions"))
    .header("Authorization", "Bearer sk-team-a")
    .header("Content-Type", "application/json")
    .POST(HttpRequest.BodyPublishers.ofString(body))
    .build()

val res = HttpClient.newHttpClient().send(req, HttpResponse.BodyHandlers.ofString())
println(res.body())
```

**Elixir** — `{:req, "~> 0.5"}`

```elixir
Req.post!("http://localhost:4000/v1/chat/completions",
  headers: [authorization: "Bearer sk-team-a"],
  json: %{model: "cheapest", messages: [%{role: "user", content: "hi"}]}
).body
```

**R** — `install.packages("httr2")`

```r
library(httr2)

request("http://localhost:4000/v1/chat/completions") |>
  req_auth_bearer_token("sk-team-a") |>
  req_body_json(list(
    model = "cheapest",
    messages = list(list(role = "user", content = "hi"))
  )) |>
  req_perform() |>
  resp_body_json()
```

**Dart / Flutter** — `dart pub add http`

```dart
import 'dart:convert';
import 'package:http/http.dart' as http;

final res = await http.post(
  Uri.parse('http://localhost:4000/v1/chat/completions'),
  headers: {
    'Authorization': 'Bearer sk-team-a',
    'Content-Type': 'application/json',
  },
  body: jsonEncode({
    'model': 'cheapest',
    'messages': [{'role': 'user', 'content': 'hi'}],
  }),
);
print(jsonDecode(res.body)['choices'][0]['message']['content']);
```

## Embed it locally — no server to run

```bash
pip install llmux      # or: npm install llmux
```

```python
import llmux
client = llmux.OpenAI()     # spawns the gateway as a local sidecar
```

In Go, llmux runs **in-process** — no subprocess at all:

```go
local, _ := llmux.Start(llmux.Options{})
defer local.Close()
// point any OpenAI Go client at local.OpenAIBaseURL()
```

## Selecting a model

- a configured **alias** — `"claude-3-5-sonnet"`
- a **provider/model** prefix — `"anthropic/claude-3-5-sonnet"`
- a **strategy** alias — `"cheapest"` (least-cost across candidates)
