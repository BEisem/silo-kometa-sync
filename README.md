# Kometa Poster Sync for Silo

A Silo plugin that mirrors what Kometa does to Plex into Silo:

- **Posters** for movies and series, including Kometa's overlays.
- **Plex collections**, with their posters and members.

It never touches season or episode artwork, so title cards from Kometa, TitleCardMaker or any other tool are left alone.

## How it works

1. Kometa finishes a run and calls the plugin's webhook (`run_end`).
2. The plugin waits a configurable delay (default 5 minutes), then syncs once. Several webhooks in a row collapse into one run.
3. For each Plex movie/TV library, it lists items carrying Kometa's `Overlay` label. Each Plex poster path ends in an upload timestamp, so only posters that changed since the last sync are sent.
4. Items are matched to Silo by TMDB id. Each poster is handed to Silo's own "apply image" endpoint, which locks the item's artwork so a Silo metadata refresh won't replace it.
5. Plex collections are mirrored as manual Silo collections. The plugin only edits or deletes collections it created.
6. If Kometa removes an item's overlay, the clean Plex poster is pushed to Silo once.

Posters are relayed through the plugin's own short-lived `/img/<random>` route rather than handing Silo a Plex URL. Silo stores the source URL of every applied image, so this keeps your Plex token out of Silo's database.

## Install

1. In Silo, create an **unscoped admin API key** (it starts with `sa_`).
2. Download `kometa-sync-linux-amd64` (or `-arm64` on ARM) from the latest [release](../../releases/latest), then in Silo go to Admin → Plugins and upload it.
3. Open the plugin's settings and fill in:
   - **Plex URL**: an address the Silo server can reach Plex at, e.g. `http://plex:32400` if both run in Docker on the same network, or `http://192.168.1.10:32400`.
   - **Plex token**.
   - **Silo admin API key**.
   - **Webhook secret**: any long random string.
4. Open the plugin's **Kometa Sync Status** page. Tick the Plex libraries to sync under **Libraries** and click Save (nothing ticked pauses syncing; libraries added to Plex later start unticked). Then click **Run check**. It tests Plex, the Overlay label, the API key and whether Silo can fetch posters from the plugin. The page also shows the webhook URL with this installation's id. (Silo's own "Check connection" button only works for metadata and marker plugins.)
5. Add the webhook to Kometa's `config.yml`, keeping any webhooks you already have:

   ```yaml
   webhooks:
     run_end: http://SILO_HOST:PORT/api/v2/plugin-content/plugins/ID/kometa?secret=YOUR_WEBHOOK_SECRET
   ```

   Copy the exact URL from the status page, which fills in this installation's id. Replace `SILO_HOST:PORT` with the address Kometa uses to reach Silo: the container name (e.g. `silo:8080`) if Kometa and Silo share a Docker network, otherwise the server's IP or hostname.

   If you already send `run_end` somewhere else, make it a list:

   ```yaml
   webhooks:
     run_end:
       - notifiarr
       - http://SILO_HOST:PORT/api/v2/plugin-content/plugins/ID/kometa?secret=YOUR_WEBHOOK_SECRET
   ```

   Watch for stray quote marks around the URL. A trailing `"` becomes part of the secret, and the plugin rejects the webhook. To test it, POST to the URL yourself (`curl -X POST -d '{}' "URL"`); the status page then shows a sync scheduled.

6. Turn on **Dry run** for the first Kometa run if you want to see what it would change. The summary appears on the status page, and Silo's task history shows the same counts for manual runs.

## Titles in more than one library

Silo has one poster per title, so when the same movie or show is in two Plex libraries (Movies and Movies 4k, say), only one copy's poster is used. Choose which on the status page's **Libraries** card:

- **Highest resolution** (default). Movies compare the actual files Plex reports. TV shows go by library name, since Plex reports no resolution for a show: a library with 4K, UHD or 2160 in its name wins.
- **Lowest resolution**. The same rules, reversed.
- **Library priority**. Number your libraries; the lowest number wins.

Changing the choice re-sends the winning poster for every shared title once, on the next sync. A poster change in a library that isn't the winner leaves Silo alone.

## Collections

Each Plex library's collections go into one Silo library, set under **Collections go to** on the Libraries card. **Automatic** matches by name ("Movies 4k" goes to a Silo library called "Movies 4K"; "TV Shows" to the only non-4K TV library, "TV"). With no match, a collection goes wherever its titles are. Changing the choice moves existing collections on the next sync; nothing is recreated.

### Separate or combined

When the same collection name exists in more than one library ("Marvel" in Movies and Movies 4k), choose on the Libraries card:

- **Separate** (default): one Silo collection per Plex library, each in its own Silo library. The 4K library's collections list only titles you have in 4K, but places that list every collection show both.
- **Combined**: one Silo collection, assigned to each of those Silo libraries, holding every title from both. Its poster comes from the library that wins under "When a title is in more than one library". Opened from the 4K library, it also lists titles you only have in 1080p.

Silo stores a title that's in both libraries as one item with two versions, so the two collections mostly hold the same titles either way. Switching modes moves existing collections on the next sync: combining keeps the winning library's collection and removes the other; separating gives the combined collection back to that library and creates the rest.

Collection order follows Plex. When Plex's order changes (for example Kometa re-ranking a Trending list), the next sync sends Silo the whole new order. In Combined mode the order comes from the copy with the most titles.

The plugin records each collection as soon as Silo creates it, rebuilds one you delete in Silo, and adopts one it created but lost track of (for example after a restart mid-sync) instead of making a duplicate.

**Reset collections** deletes the plugin's collections for the libraries you choose, including any it lost track of, and leaves posters alone. The next sync rebuilds them. Collections you made yourself in Silo are never touched; the plugin only removes collections whose Silo slug starts with `kometa-`.

## While it runs

The status page shows the current step (reading Plex, applying posters, syncing collections) with a running count, and updates by itself. Times show in your own time zone.

If Silo re-keys a title during a sync (it does this when it matches a show better, e.g. `series-imdb-…` becomes `series-tvdb-…`), the plugin looks the title up again and retries. A title Silo no longer has counts under "Not in Silo" rather than as an error.

## The scheduled task

The plugin also registers a **Kometa Poster Sync** task in Silo's task list. That's the "Run now" button. Silo gives plugin tasks a run-at-startup trigger by default, which is cheap because only changed posters are sent. You can switch that trigger to a daily fallback or leave it. The webhook is what keeps it in step with Kometa, however often you run it.

## Reverting

Silo has no bulk "reset artwork" action, and the image lock it sets when a poster is applied makes library refreshes skip those items. The status page's **Revert** section handles it per library:

1. Untick the library under **Libraries** and save first, or the next Kometa run pushes the posters again.
2. Tick the libraries, optionally tick "Also delete the Silo collections this plugin created", tick the confirmation, and submit.

For each item the plugin changed, it removes only the image lock (your other locks stay), runs Silo's own metadata refresh so Silo picks its default poster again, and waits for that job before starting more (three at a time). Progress shows on the status page.

A movie in both Movies and Movies 4k is one Silo item, so reverting either library resets it. If you had hand-picked a poster in Silo before the plugin replaced it, revert gives you Silo's default, not that earlier pick.

## Build from source

Requires Go 1.26.

```sh
go mod tidy
make build      # dist/kometa-sync-linux-amd64 and -arm64
make test
```

## Limits worth knowing

- Items Plex has no TMDB id for are skipped and counted under "No TMDB id".
- Plex "composite" collection posters (the auto collage) aren't copied. Silo builds its own.
- A collection whose items aren't in Silo yet is skipped until they are.
- Posters over ~3.8 MB are re-encoded to JPEG for the relay.

## Releasing

Change `"version"` in `manifest.json` (and `VERSION` in the Makefile) and push to `main`. GitHub Actions runs the tests, builds both Linux binaries and publishes them as release `v<version>`. You can also start it by hand from the Actions tab ("Release" → "Run workflow"); it does nothing if that version is already released.

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE). The bundled Plus Jakarta Sans font is under the SIL Open Font License (`assets/OFL-PlusJakartaSans.txt`).
