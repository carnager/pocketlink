package dev.tether

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.graphics.drawable.Icon
import android.media.MediaMetadata
import android.media.VolumeProvider
import android.media.session.MediaSession
import android.media.session.PlaybackState
import android.os.Handler
import android.os.Looper
import android.os.SystemClock
import android.util.Log
import java.util.concurrent.Executors
import kotlin.math.abs
import okhttp3.Request
import org.json.JSONObject

/**
 * Remote control for one computer's media players: a media session and a
 * media-style notification mirroring the computer's active player. All
 * session work happens on the main thread.
 */
class Remote(private val ctx: Context, private val link: Link) {
    private val main = Handler(Looper.getMainLooper())
    private val fetcher = Executors.newSingleThreadExecutor { Thread(it, "tether-art") }
    private val nm = ctx.getSystemService(NotificationManager::class.java)
    private val notifId = NOTIF_BASE + abs(link.id.hashCode() % 1000)

    private var session: MediaSession? = null
    private var player: JSONObject? = null
    private var artKey: String? = null
    private var art: Bitmap? = null

    private val volume = object : VolumeProvider(VOLUME_CONTROL_ABSOLUTE, 100, 50) {
        override fun onSetVolumeTo(volume: Int) {
            currentVolume = volume.coerceIn(0, 100)
            command("volume", currentVolume / 100.0)
        }

        override fun onAdjustVolume(direction: Int) = onSetVolumeTo(currentVolume + direction * 5)
    }

    /** Applies a media.state frame from the computer. */
    fun update(state: JSONObject) = main.post {
        val active = state.optString("active")
        val players = state.optJSONArray("players")
        player = (0 until (players?.length() ?: 0)).map { players!!.getJSONObject(it) }.find { it.optString("id") == active }
        render()
    }

    /** Hides the controls, e.g. while disconnected, when state may be stale. */
    fun clear() = main.post {
        player = null
        render()
    }

    fun command(action: String, value: Double? = null) {
        val p = player ?: return
        link.sendLive("media.cmd", JSONObject().put("player", p.optString("id")).put("action", action).apply {
            if (value != null) put("value", value)
        })
    }

    private fun render() {
        val p = player
        if (p == null) {
            nm.cancel(notifId)
            session?.release()
            session = null
            return
        }
        val s = session ?: MediaSession(ctx, "tether-${link.id.take(8)}").also {
            it.setCallback(callback, main)
            it.isActive = true
            session = it
        }

        val key = p.optString("art").ifEmpty { null }
        if (key != artKey) {
            artKey = key
            art = null
            if (key != null) fetchArt(key)
        }

        val title = p.optString("title").ifEmpty { p.optString("name") }
        val artist = p.optString("artist")
        s.setMetadata(
            MediaMetadata.Builder()
                .putString(MediaMetadata.METADATA_KEY_TITLE, title)
                .putString(MediaMetadata.METADATA_KEY_ARTIST, artist)
                .putString(MediaMetadata.METADATA_KEY_ALBUM, p.optString("album"))
                .putLong(MediaMetadata.METADATA_KEY_DURATION, p.optLong("length_ms"))
                .apply { art?.let { putBitmap(MediaMetadata.METADATA_KEY_ART, it) } }
                .build()
        )

        val playing = p.optString("status") == "Playing"
        var actions = PlaybackState.ACTION_PLAY_PAUSE or PlaybackState.ACTION_PLAY or PlaybackState.ACTION_PAUSE
        if (p.optBoolean("can_next")) actions = actions or PlaybackState.ACTION_SKIP_TO_NEXT
        if (p.optBoolean("can_prev")) actions = actions or PlaybackState.ACTION_SKIP_TO_PREVIOUS
        if (p.optBoolean("can_seek")) actions = actions or PlaybackState.ACTION_SEEK_TO
        s.setPlaybackState(
            PlaybackState.Builder()
                .setActions(actions)
                .setState(
                    if (playing) PlaybackState.STATE_PLAYING else PlaybackState.STATE_PAUSED,
                    p.optLong("position_ms"), if (playing) 1f else 0f, SystemClock.elapsedRealtime(),
                )
                .build()
        )

        // Hardware volume keys control the computer's player while this session is active.
        val vol = p.optDouble("volume", -1.0)
        if (vol >= 0) {
            volume.currentVolume = (vol * 100).toInt()
            s.setPlaybackToRemote(volume)
        }

        nm.notify(notifId, notification(s, p, title, artist, playing))
    }

    private fun notification(s: MediaSession, p: JSONObject, title: String, artist: String, playing: Boolean): Notification {
        val compact = mutableListOf<Int>()
        val b = Notification.Builder(ctx, Notifs.MEDIA)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(title)
            .setContentText(artist)
            .setSubText("${p.optString("name")} on ${link.name}")
            .setLargeIcon(art)
            .setVisibility(Notification.VISIBILITY_PUBLIC)
            .setOngoing(playing)
            .setContentIntent(PendingIntent.getActivity(ctx, 0, Intent(ctx, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE))

        fun add(icon: Int, label: String, action: String) {
            compact += compact.size
            b.addAction(Notification.Action.Builder(Icon.createWithResource(ctx, icon), label, actionIntent(action)).build())
        }
        if (p.optBoolean("can_prev")) add(R.drawable.ic_skip_previous, "Previous", "previous")
        if (playing) add(R.drawable.ic_pause, "Pause", "pause") else add(R.drawable.ic_play, "Play", "play")
        if (p.optBoolean("can_next")) add(R.drawable.ic_skip_next, "Next", "next")

        return b.setStyle(Notification.MediaStyle().setMediaSession(s.sessionToken).setShowActionsInCompactView(*compact.toIntArray()))
            .build()
    }

    private fun actionIntent(action: String): PendingIntent = PendingIntent.getBroadcast(
        ctx, (link.id + action).hashCode(),
        Intent(ctx, MediaActionReceiver::class.java).putExtra(EXTRA_LINK, link.id).putExtra(EXTRA_ACTION, action),
        PendingIntent.FLAG_IMMUTABLE,
    )

    private fun fetchArt(key: String) = fetcher.execute {
        val (base, client) = link.target ?: return@execute
        val bitmap = try {
            client.newCall(Request.Builder().url("$base/media/art/$key").build()).execute().use { r ->
                if (!r.isSuccessful) return@use null
                BitmapFactory.decodeStream(r.body.byteStream())?.let(::shrink)
            }
        } catch (e: Exception) {
            Log.w("tether", "fetching album art", e)
            null
        }
        main.post {
            if (artKey == key && bitmap != null) {
                art = bitmap
                render()
            }
        }
    }

    private fun shrink(b: Bitmap): Bitmap {
        val max = 512
        if (b.width <= max && b.height <= max) return b
        val scale = max.toFloat() / maxOf(b.width, b.height)
        return Bitmap.createScaledBitmap(b, (b.width * scale).toInt(), (b.height * scale).toInt(), true)
    }

    private val callback = object : MediaSession.Callback() {
        override fun onPlay() = command("play")
        override fun onPause() = command("pause")
        override fun onSkipToNext() = command("next")
        override fun onSkipToPrevious() = command("previous")
        override fun onSeekTo(pos: Long) = command("set_position", pos.toDouble())
    }

    companion object {
        const val EXTRA_LINK = "dev.tether.LINK"
        const val EXTRA_ACTION = "dev.tether.ACTION"
        private const val NOTIF_BASE = 2000
    }
}

/** Handles the buttons on media notifications. */
class MediaActionReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        val link = Links.get(ctx).get(intent.getStringExtra(Remote.EXTRA_LINK) ?: return) ?: return
        link.remote.command(intent.getStringExtra(Remote.EXTRA_ACTION) ?: return)
    }
}
