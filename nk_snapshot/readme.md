## Running `nk-snap`

Place the compiled `nk-snap` binary on the NanoKVM and ensure the credentials file `/etc/nanokvm.snapshot.conf` exists with the following format:

KVM_USER=your_username
KVM_PASS=your_password

Set secure permissions on the file:

chmod 600 /etc/nanokvm.snapshot.conf

Make the binary executable and run it:

chmod +x ./nk-snap
./nk-snap

Once running, the service maintains a persistent connection to the HDMI MJPEG stream and exposes a local snapshot endpoint. Any process on the NanoKVM can obtain a capture of the current HDMI frame by requesting:

http://127.0.0.1:18080/snapshot.jpg

The endpoint returns a JPEG image of the latest frame with minimal latency, without reopening the video stream for each request.