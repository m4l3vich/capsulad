# Cadence Server for `capsulad` setup guide

You just need Docker, some patience and this folder (capsulad-cadence-server) on your target machine (the one you will be deploying the server to).

## Deploying the Cadence Server
Build the `cadence-server` and `cadence-dashboard` images from sources:
```sh
git clone https://git.shino.boo/devilreef/cadence
cd cadence && mkdir ../cadence-src && git archive HEAD | tar -x -C ../cadence-src
docker build -f server/Dockerfile --build-arg VERSION=$(git rev-parse --short HEAD) -t cadence-server:local ../cadence-src
docker build -f deploy/dashboard/Dockerfile -t cadence-dashboard:local .
```

After a successful build, remove the cadence/ and cadence-src/ directories.

Change the values in your .env according to your setup (if not sure, replace the value in CADENCE_HOST to your local IP address), then run:
```sh
docker compose run setup
```

After the certs are generated, launch the server stack:
```sh
docker compose up -d
```

Then set an admin password for the dashboard:
```sh
sh ./setup/make-password.sh
```

Then open `https://(CADENCE_HOST):(CADENCE_PORT)/` in your browser, log in with your password and do the first-time setup.

## CA certificate exchange
`capsulad` has a self-signed TLS certificate and `cadence-server` now has its own one. They need to trust each other's certificates in order for everything to work properly. So:

### Add `cadence-server`'s cert to `capsulad`
Get the `./data/ca.crt` in Cadence deployment folder, open `capsulad`'s Web UI, open the "Files" section, and upload this file to `/assets/cadence/ca.crt`.

### Add `capsulad`'s cert to `cadence-server`
Open `capsulad`'s "Access" settings section in Web UI, click "Download CA" and put the file it gives you to `./data/ca-certs/` in Cadence deployment folder.

## Connecting to `capsulad`
Install "Cadence Client" app to your `capsulad` device, open its settings and follow the instructions to set up a connection.

# Cadence Server maintenance

## Updating Cadence Server
It's as easy as running the building commands again:
```sh
# Build again
git clone https://git.shino.boo/devilreef/cadence
cd cadence && mkdir ../cadence-src && git archive HEAD | tar -x -C ../cadence-src
docker build -f server/Dockerfile --build-arg VERSION=$(git rev-parse --short HEAD) -t cadence-server:local ../cadence-src
docker build -f deploy/dashboard/Dockerfile -t cadence-dashboard:local .

# Deploy the latest stack
docker compose up -d
```

Don't forget to remove the cadence/ and cadence-src/ directories.

## Changing `CADENCE_HOST` or `CADENCE_PORT`
After you've changed the values, you need to generate the certificates again:
```sh
docker compose stop
docker compose run setup
docker compose start
```

Then take the `./data/ca.crt` and upload it to `capsulad` again.