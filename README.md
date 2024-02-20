# EchoVR Game Server

A wrapper for running EchoVR on Agones.

It consists of a docker image and wrapper that:

* Calls the EchoVR executable with arguments based on Argones.
* Runs health checks on EchoVR to ensure it's up.
* Combines stdout, stderr and logging output of the application to stderr from the server.
* Registers with Nakama to provide allocatable resources.
* Provides a proxied, cached access to the HTTP session API.

## Features

### Output Capture

The wrapper will capture both stdout, stderr. It will also create a FIFO for EchoVR.exe to write it's log to. The wrapper will output stderr, prefixing game engine output as such:

*NOTE:* It will also filter some extraneous messages.

### Fault Detection

The wrapper starts a TCP proxy, and instructs EchoVR.exe to connect to the wrapper's internal TCP proxy for the login service connection. IF this connection is closed by either party, the wrapper will gracefully shutdown the game and exit.

**NOTE:** This wrapper will not restart the app.

### HTTP API Caching

The wrapper runs a caching proxy that is rate limited to 30 requests per second to EchoVR's /session. Once the rate limit is exceed, the request is fulfilled from cache.

## Configuration

The server has a few configuration options that can be set via command line
flags. Some can also be set using environment variables.

| Flag                                   | Environment Variable | Default |
|----------------------------------------|----------------------|---------|
| ready                                  | READY                | true    |
| automaticShutdownDelaySec              | _n/a_                | 0       |
| automaticShutdownDelayMin (deprecated) | _n/a_                | 0       |
| readyDelaySec                          | _n/a_                | 0       |
| readyIterations                        | _n/a_                | 0       |
| timeStepFrequency                      | _n/a_                | 120     |

## Ideas

### Wrapper

* Handle state and session. The wrapper sends requests on behalf of the broadcaster to nakama.
* Regular Bandwidth measurement and latency monitoring.
* Add more configurable server arguments like region.
* Start the server with a specific game mode or level.
  