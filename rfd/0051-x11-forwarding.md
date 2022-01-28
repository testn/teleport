---
authors: Brian Joerger (bjoerger@goteleport.com)
state: implemented
---

# RFD 51 - X11 Forwarding

## What

X11 forwarding makes it possible to open a graphical application on a remote
machine and but forward its graphical (X) data to the client's local display.  
SSH rfc [4254](https://www.rfc-editor.org/rfc/rfc4254#section-6.3) details the
protocol for supporting X11 forwarding in an SSH server/client.

## Why

X11 forwarding is used by some SSH users who are switching over to Teleport.
Currently, we only support X11 forwarding between an OpenSSH client and server
in proxy recording mode. As user's move more of their remote fleet to Teleport,
there is a growing demand for supporting X11 between any combination of OpenSSH
server/client and Teleport server/client.

## Details

X11 forwarding is a sparsely documented protocol with many hidden intricacies.
In implementing X11 forwarding for Teleport I leaned heavily upon OpenSSH
implementation, which is (was) the only complete open source X11 forwarding
implementation.

This RFD will explain some key concepts and temrs for X11 forwarding, the general
flow of the implementation, and the security details and decisions.

### X Server and Client

An X Server is an application on a machine which manages graphical displays, input
devices, etc. Linux systems ship with an XServer (XOrg), while other operating
systems require a standalone XServer (ex. XQuartz for Mac, XMing for Windows).

An X Client is a application which interacts with an X server to send/receive
graphical input and output. For Max and Linux, most terminals have an X Client
built in. On Windows, you have to install a standalone X Client such as MobaXTerm.

When an X Client makes an X request, it parses the set `$DISPLAY` to send an X request
to a specific display opened in the X Server. the first portion of this request is
an authorization packet used to authenticate and authorize the X Client.

The X Client gets the authorization packet from `$XAUTHORITY` or `~/.Xauthority`.
If there is no matching authorization data in the file, or it doesn't match what
the X Server expected, then the X request will be denied.

### X11 Forwarding flow

1. Client opens an SSH session and then sends an `x11-req` ssh request to the server. 
   The `x11-req` includes an X authentication protocol and cookie, which will be detailed
   in the security sections below.
2. Server receives request and either approves or denies the request.
3. If the server approves, it will open an XServer proxy listener to catch any
   XServer requests within the SSH session.
4. Upon receiving an XServer request on the proxy listener, the server sends an
   `x11` channel request to the client.
5. If the client accepts, the client will start X11 forwarding between the x11
   channel and the client's local display - `$DISPLAY`.
6. Server begins X11 forwarding between the x11 channel and XServer proxy connection.
7. The XServer request is now connected to the client's local display. For example, if
   `xeyes` was run, then the program would be running on the server, but any graphical
   input/output would the client's local XServer.

### X Server Proxy

In step 3 above, the SSH server opens an X Server Proxy. More specifically, the server
starts listening on a unix socket so that it can accept requests to the corresponding
display. 

First, the server has to find an open display socket. A display's unix socket can be 
determined by its display number - `/tmp/.X11-unix/X<display_number>`. So we start
searching for an open display number between 10 and 1009.

Note: We use the same display offset (10) and max displays (1000) as OpenSSH.

During session shell creation, we can set `$DISPLAY` and X authorization data for the
session. The `$DISPLAY` is set to `unix:<display_number>`, which will be translated
to the open socket during an X Client request. X Authorization data will be set in
the user's default xauthfile (`~/.Xauthority`) by calling 
`xauth add <$DISPLAY> <x11-req.proto> <x11-req.cookie>`. 

### Security

There are four points of contact which concern security within the X11 forwarding flow
above:

1. Client sends `x11-req` to server to allow X11 forwarding during the Session.
2. Session user makes an X Client request to the SSH Server's proxy X Server.
3. Server requests to open an `x11` channel to client to forward X Client request.
4. Client forward X Client request to local X Server.

#### Server Authorization - X11 Forwarding Request

The initial X11 forwarding request made by the client will be denied by the server in
the following cases:

 - The Server does not have X11 forwarding enabled in its `teleport.yaml`.
 - The Server does not have an X Authority configured, specifically the `xauth` binary.
 - The Client's SSH certificate does not have the `permit-X11-forwarding` extension.

If the request is authorized, then the Server will read the X Authorization protocol and
cookie attached in the request and set `$DISPLAY` and `~/.Xauthority` as explained above.

#### Session Authorization - X Client Request

Once X11 forwarding is enabled for a session, the Session user can make an X Client
request and start X11 forwarding. As explained in the X Server Proxy section, the 
SSH Session's X Client requests will leverage the set `$DISPLAY` and the X Authorization
data set in `~/.Xauthority` to connect to the X Server Proxy.

If a user attempts to send an X Client request for the `$DISPLAY` with a different xauth
file, it will be denied. However it's important to note that any remote user that does
have access to `~/.Xauthority` will be able to perform X11 forwarding as if they were
in the session. This is where understanding `trusted` vs `untrusted` forwarding is 
very important, which have their own sections below.

#### Client Authentication - X11 Channel Request

When the SSH client receives an `x11` channel request, it must authenticate the server
before providing access to the client's local X Server. To do this, it will use the
X authorization data it sent in the original `x11-req`

Note: The X authorization data sent in the `x11-req` is fake X authorization data
created by the server. 

First, the client sends fake X authorization data to the server in the `x11-req` request.
As detailed in the SSH RFC, this data includes an X authorization protocol and a random
cookie of that protocol. We use the `MIT-MAGIC-COOKIE-1` protocol, which calls for a random
128 bit hexadecimal encoded cookie. This is the simplest and most common protocol.

When the client receives an `x11` SSH channel request from the server, it mimics and
X Server by scanning the connection for its X authorization packet, the first portion
of the request. If the X authorization packet includes the protocol and cookie sent by
the client, then the server will be trusted.

Note: The X authorization data used in this step is fake and does not actually provide
access to the client's XServer. It's essential that an XServer's X authorization data
is never shared outside of the device, or else the XServer can be compromised.

#### Client X Server Authorization

To complete X11 forwarding, the client must start forwarding between the `x11` channel
and the local X Server. However, the X authorization data in the `x11` channel request
was the fake data we created. So before forwarding the X request to the X Server, the
SSH client replaces the fake data with real X authorization data. This data will either
be trusted xauth data, or untrusted xauth data, depending on what the client requested.

##### Untrusted X11 Forwarding

Untrusted X authorization data must be generated through `xauth` in order to be recognized
by the X Server. To do this, the client runs `xauth generate <$DISPLAY> untrusted <timeout?>`.
Any X Client request using this untrusted token will be be subject to the X Security extension,
granting it fewer privileges than usual. 

##### Trusted X11 Forwarding

In trusted X11 forwarding, we can rely on the client's local environment. If we fill in 
the X authorization packet in the request with a random cookie, the X Server will just
ignore it and connect as if it received the request locally.

### tsh UX

`tsh ssh -X` or `tsh ssh -Y` can be used to request a session with `trusted` X11 forwarding.
However, if `tsh ssh -X` is used with the `-oForwardX11Trusted=yes` flag, it will instead
request `untrusted` forwarding, while `tsh ssh -Y` would still request `trusted`.

This may seem unintuitive, and it is, but this is the same UX as OpenSSH client. In OpenSSH,
to use untrusted X11 forwarding you have to set the `ForwardX11Trusted no` in the client's
ssh config, so we simply mimic that behavior with our existing OpenSSH flag options.

The reason for this oddity is essentially that `trusted` X11 forwarding (aka the X Security
extension) is an opt in extension of the X Protocol with negative performance and compatibility
implications.

### X Security extension

The X Security extension was created about 10 years after the 11th version of X. The
extension introduced `untrusted` tokens which could be used to grant fewer X privileges
to the user of the token. It also introduced the ability to set a timeout for the token.

In contrast, normal `trusted` tokens give full unmitigated X privileges, allowing for action
such as screenshots, monitoring peripherals, and many other actions one would expect to
perform on local machines. In the case of X11 forwarding however, this means that the
client is providing the SSH server unmitigated access to its local X Server. If the server
becomes compromised or another user has access to your remote user, then the Client's
local XServer could be subject to attacks such as keystroke monitoring to capture a password.

Unfortunately, since the X Security extension was created as an afterthought, it is not
built into the X protocol and has some serious performance drawbacks. The performance 
drop is somewhere in the ballpark of 10x. Additionally, many X Client's don't support
the X Security Extension properly and may crash immediately or at some point during 
the connection.