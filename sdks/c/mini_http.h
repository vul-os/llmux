/*
 * mini_http.h — just enough HTTP/1.1 over loopback for the sidecar examples.
 *
 * C has no HTTP client in its standard library, and the house rule for these
 * SDKs is to prefer the standard library over a third-party dependency. So
 * these examples talk to the sidecar with BSD sockets directly. That is honest
 * for the case it covers — a request to 127.0.0.1, over a connection this
 * process opened, with `Connection: close` — and it is NOT a general HTTP
 * client. It does not do TLS, redirects, chunked transfer-encoding,
 * keep-alive, proxies, IPv6 literals or retries.
 *
 * If you are writing a real program: link libcurl. This file exists so the
 * examples have no dependencies, not as a component to reuse.
 *
 * SPDX-License-Identifier: MIT OR Apache-2.0
 */

#ifndef MINI_HTTP_H
#define MINI_HTTP_H

#include <stddef.h>

typedef struct {
	int status;  /* HTTP status code, or 0 if the request never completed */
	char *body;  /* NUL-terminated; free with http_response_free */
	size_t len;
} http_response;

/*
 * One request/response against 127.0.0.1:port. body may be NULL for GET.
 * Returns 0 on success (a response arrived, whatever its status), -1 on a
 * transport failure with errbuf filled in.
 *
 * On success the caller owns out->body and must call http_response_free.
 */
int http_request(int port, const char *method, const char *path, const char *bearer,
                 const char *body, http_response *out, char *errbuf, size_t errcap);

void http_response_free(http_response *r);

/*
 * POST and read a server-sent-event stream, calling on_event once per `data: `
 * line, as the frames arrive rather than after the response completes. `data`
 * is NUL-terminated and valid only for the duration of the call.
 *
 * Return non-zero from on_event to stop reading and close the connection.
 * Returns 0 on success, -1 on a transport failure with errbuf filled in.
 *
 * The terminal `data: [DONE]` frame is delivered like any other; the caller
 * decides what it means, exactly as it must against the real API.
 */
int http_sse(int port, const char *path, const char *bearer, const char *body,
             int (*on_event)(const char *data, void *user_data), void *user_data,
             char *errbuf, size_t errcap);

/*
 * Ask the kernel for an unused loopback port and give it straight back.
 *
 * Racy by construction: another process can take the port between the close
 * here and the child's bind. Every SDK in this repo does the same thing for the
 * same reason — there is no portable "reserve a port for my child" — and the
 * window is small enough that the health poll catches the loss as a startup
 * failure rather than a mystery. Returns 0 on failure.
 */
int http_free_port(void);

#endif /* MINI_HTTP_H */
