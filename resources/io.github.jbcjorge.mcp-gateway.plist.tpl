<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>io.github.jbcjorge.mcp-gateway</string>

    <key>ProgramArguments</key>
    <array>
        <string>__BINARY__</string>
        <string>__CONFIG__</string>
    </array>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>__PATH__</string>
        <key>HOME</key>
        <string>__HOME__</string>
    </dict>

    <!-- Socket activation: launchd owns the port, passes to binary via launch_activate_socket("mcp") -->
    <key>Sockets</key>
    <dict>
        <key>mcp</key>
        <dict>
            <key>SockServiceName</key>
            <string>__PORT__</string>
            <key>SockType</key>
            <string>stream</string>
            <key>SockNodeName</key>
            <string>127.0.0.1</string>
        </dict>
    </dict>

    <key>StandardErrorPath</key>
    <string>__LOGDIR__/mcp-gateway.stderr.log</string>

    <!-- Don't start at login - only on first socket connection -->
    <key>RunAtLoad</key>
    <false/>
</dict>
</plist>
