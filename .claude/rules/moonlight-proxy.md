---
paths:
  - pkg/moonlight/**
  - cmd/moonlight-proxy/**
---

# moonlight-proxy

- An error the Moonlight user should read goes out as HTTP 200 with the error in the XML
  `status_code`/`status_message` (as Sunshine does), via `sendXMLWithHTTPStatus`.
  - moonlight-qt turns a non-2xx HTTP status into a transport error and never shows the message.
- `--launch-timeout` must stay under moonlight-qt's 120s launch request timeout, or the client
  gives up first and shows a generic error while the session keeps starting.
- The session limit (`createSession`) holds only because the check and the Create share
  `launchMu` and count from a live `List`, not the informer.
  - It is per process: running more than one moonlight-proxy replica breaks it.
  - Tests: `TestLaunchConcurrentUsersRespectLimit` in `pkg/moonlight/launch_test.go`.
- `RESTServerOptions.BusyCheck` is the hook for anything besides Sessions that holds the GPU
  (the Library pod). A check error fails the launch closed.
