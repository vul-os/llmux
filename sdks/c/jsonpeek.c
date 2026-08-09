/* jsonpeek.c — see jsonpeek.h. SPDX-License-Identifier: MIT OR Apache-2.0 */

#include "jsonpeek.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Find `"key":` and return a pointer just past the colon, skipping spaces. */
static const char *seek(const char *doc, const char *key) {
	char needle[128];
	int n = snprintf(needle, sizeof(needle), "\"%s\":", key);
	if (n < 0 || (size_t)n >= sizeof(needle)) return NULL;
	const char *at = strstr(doc, needle);
	if (!at) return NULL;
	at += strlen(needle);
	while (*at == ' ') at++;
	return at;
}

/* Copy an escaped JSON string body (p points just past the opening quote) into
 * out at offset *len. Returns a pointer past the closing quote. */
static const char *copy_string(const char *p, char *out, size_t outcap, size_t *len) {
	while (*p && *p != '"') {
		char c = *p;
		if (c == '\\' && p[1]) {
			p++;
			switch (*p) {
			case 'n': c = '\n'; break;
			case 't': c = '\t'; break;
			case 'r': c = '\r'; break;
			default: c = *p; break; /* \" \\ \/ and, unhandled, \uXXXX */
			}
		}
		if (*len + 1 < outcap) {
			out[(*len)++] = c;
			out[*len] = '\0';
		}
		p++;
	}
	return *p == '"' ? p + 1 : p;
}

int json_string(const char *doc, const char *key, char *out, size_t outcap) {
	if (outcap) out[0] = '\0';
	const char *at = seek(doc, key);
	if (!at || *at != '"') return 0;
	size_t len = 0;
	copy_string(at + 1, out, outcap, &len);
	return 1;
}

int json_string_append_all(const char *doc, const char *key, char *out, size_t outcap) {
	char needle[128];
	int n = snprintf(needle, sizeof(needle), "\"%s\":\"", key);
	if (n < 0 || (size_t)n >= sizeof(needle)) return 0;
	size_t len = strlen(out);
	int found = 0;
	const char *p = doc;
	while ((p = strstr(p, needle)) != NULL) {
		p += strlen(needle);
		p = copy_string(p, out, outcap, &len);
		found++;
	}
	return found;
}

int json_number(const char *doc, const char *key, double *out) {
	const char *at = seek(doc, key);
	if (!at) return 0;
	char *end = NULL;
	double value = strtod(at, &end);
	if (end == at) return 0;
	*out = value;
	return 1;
}
