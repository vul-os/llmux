#!/usr/bin/env ruby
# frozen_string_literal: true

# Test fixture: a fake llmux binary.
# Honors LLMUX_ADDR=127.0.0.1:<port>, binds it, serves GET /health -> 200.
#   FAKE_HEALTH_STATUS  status for /health (default 200)
#   FAKE_NEVER_LISTEN   if "1", never binds (simulates a hung start)

require "socket"

trap("TERM") { exit 0 }
trap("INT") { exit 0 }

if ENV["FAKE_NEVER_LISTEN"] == "1"
  sleep 30
  exit 0
end

addr = ENV["LLMUX_ADDR"] || "127.0.0.1:0"
host, port = addr.split(":")
status = (ENV["FAKE_HEALTH_STATUS"] || "200").to_i
reason = status == 200 ? "OK" : "Unavailable"

server = TCPServer.new(host, port.to_i)
loop do
  conn = server.accept
  request_line = conn.gets.to_s
  # Drain headers.
  while (line = conn.gets) && line != "\r\n"; end
  if request_line.start_with?("GET /health")
    conn.write "HTTP/1.1 #{status} #{reason}\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"
  else
    conn.write "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
  end
  conn.close
rescue StandardError
  conn&.close
end
