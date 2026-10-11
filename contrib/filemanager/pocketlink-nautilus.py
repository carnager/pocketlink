# "Send to phone" in Nautilus' context menu. Needs nautilus-python
# (nautilus-python on Arch); install to /usr/share/nautilus-python/extensions/
# or ~/.local/share/nautilus-python/extensions/.

from gi.repository import GLib, GObject, Nautilus


class PocketlinkSend(GObject.GObject, Nautilus.MenuProvider):
    # Nautilus 43+ passes (files); older versions (window, files).
    def get_file_items(self, *args):
        files = args[-1]
        if not files or any(f.get_uri_scheme() != "file" for f in files):
            return []
        item = Nautilus.MenuItem(name="Pocketlink::send", label="Send to phone", icon="smartphone")
        item.connect("activate", self._send, files)
        return [item]

    def _send(self, _item, files):
        paths = [f.get_location().get_path() for f in files]
        # GLib reaps the child, so finished sends don't linger as zombies.
        GLib.spawn_async(["pocketlink", "send", "-notify", "--"] + paths, flags=GLib.SpawnFlags.SEARCH_PATH)
