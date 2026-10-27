package io.github.carnager.pocketlink

import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.media.AudioAttributes
import android.media.AudioManager
import android.media.MediaPlayer
import android.media.RingtoneManager
import android.os.BatteryManager
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.VibrationEffect
import android.os.Vibrator
import android.os.VibratorManager
import android.util.Log
import androidx.core.content.ContextCompat
import org.json.JSONObject

private const val TAG = "pocketlink"

/**
 * Rings the phone so it can be found: the ringtone on the alarm stream at
 * full volume, which plays even on silent, until stopped from the
 * notification, by a computer, or after [MAX_MS].
 */
object FindPhone {
    private const val NOTIF_TAG = "find"
    private const val MAX_MS = 2 * 60 * 1000L

    private val main = Handler(Looper.getMainLooper())
    private var player: MediaPlayer? = null
    private var vibrator: Vibrator? = null
    private var savedVolume = -1
    private var app: Context? = null
    private val timeout = Runnable { app?.let(::stop) }

    fun ring(ctx: Context, computer: String): Boolean = main.post {
        val c = ctx.applicationContext
        app = c
        if (player == null) {
            val am = c.getSystemService(AudioManager::class.java)
            try {
                savedVolume = am.getStreamVolume(AudioManager.STREAM_ALARM)
                am.setStreamVolume(AudioManager.STREAM_ALARM, am.getStreamMaxVolume(AudioManager.STREAM_ALARM), 0)
            } catch (e: SecurityException) {
                Log.w(TAG, "raising the alarm volume", e)
                savedVolume = -1
            }
            player = play(c)
            vibrator = vibrate(c)
        }
        notify(c, computer)
        main.removeCallbacks(timeout)
        main.postDelayed(timeout, MAX_MS)
    }

    fun stop(ctx: Context): Boolean = main.post {
        val c = ctx.applicationContext
        main.removeCallbacks(timeout)
        player?.run {
            stop()
            release()
        }
        player = null
        vibrator?.cancel()
        vibrator = null
        if (savedVolume >= 0) {
            try {
                c.getSystemService(AudioManager::class.java).setStreamVolume(AudioManager.STREAM_ALARM, savedVolume, 0)
            } catch (e: SecurityException) {
                Log.w(TAG, "restoring the alarm volume", e)
            }
            savedVolume = -1
        }
        c.getSystemService(NotificationManager::class.java).cancel(NOTIF_TAG, 0)
    }

    /** The ringtone, else the alarm sound, looped; null if neither plays. */
    private fun play(ctx: Context): MediaPlayer? {
        val attrs = AudioAttributes.Builder()
            .setUsage(AudioAttributes.USAGE_ALARM)
            .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
            .build()
        for (type in intArrayOf(RingtoneManager.TYPE_RINGTONE, RingtoneManager.TYPE_ALARM, RingtoneManager.TYPE_NOTIFICATION)) {
            val uri = RingtoneManager.getActualDefaultRingtoneUri(ctx, type) ?: continue
            val p = MediaPlayer()
            try {
                p.setAudioAttributes(attrs)
                p.setDataSource(ctx, uri)
                p.isLooping = true
                p.prepare()
                p.start()
                return p
            } catch (e: Exception) {
                Log.w(TAG, "playing $uri", e)
                p.release()
            }
        }
        return null
    }

    private fun vibrate(ctx: Context): Vibrator? {
        val v = if (Build.VERSION.SDK_INT >= 31) {
            ctx.getSystemService(VibratorManager::class.java)?.defaultVibrator
        } else {
            @Suppress("DEPRECATION")
            ctx.getSystemService(Vibrator::class.java)
        }
        if (v == null || !v.hasVibrator()) return null
        v.vibrate(VibrationEffect.createWaveform(longArrayOf(0, 800, 400), 0))
        return v
    }

    private fun notify(ctx: Context, computer: String) {
        val stop = PendingIntent.getBroadcast(
            ctx, 0, Intent(ctx, FindStopReceiver::class.java), PendingIntent.FLAG_IMMUTABLE,
        )
        val n = Notification.Builder(ctx, Notifs.FIND)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle("$computer is looking for this phone")
            .setContentText("Tap to stop ringing")
            .setContentIntent(stop)
            .setDeleteIntent(stop)
            .addAction(Notification.Action.Builder(null, "Stop", stop).build())
            .setCategory(Notification.CATEGORY_ALARM)
            .setOngoing(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(NOTIF_TAG, 0, n)
    }
}

class FindStopReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        FindPhone.stop(ctx)
    }
}

/** Tells the computers the battery level and whether it's charging. */
object Battery {
    private var last: Pair<Int, Boolean>? = null

    /** Follows battery changes for as long as the app runs. */
    fun watch(ctx: Context, links: Links) {
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(c: Context, intent: Intent) {
                val s = state(intent) ?: return
                // The broadcast also fires for temperature and voltage.
                val key = s.getInt("level") to s.getBoolean("charging")
                if (key == last) return
                last = key
                links.all().forEach { it.sendLive("battery", s) }
            }
        }
        ContextCompat.registerReceiver(ctx, receiver, IntentFilter(Intent.ACTION_BATTERY_CHANGED), ContextCompat.RECEIVER_NOT_EXPORTED)
    }

    /** The battery right now, for a new connection. */
    fun current(ctx: Context): JSONObject? =
        state(ContextCompat.registerReceiver(ctx, null, IntentFilter(Intent.ACTION_BATTERY_CHANGED), ContextCompat.RECEIVER_NOT_EXPORTED))

    private fun state(intent: Intent?): JSONObject? {
        val level = intent?.getIntExtra(BatteryManager.EXTRA_LEVEL, -1) ?: return null
        val scale = intent.getIntExtra(BatteryManager.EXTRA_SCALE, 100)
        if (level < 0 || scale <= 0) return null
        val status = intent.getIntExtra(BatteryManager.EXTRA_STATUS, -1)
        val charging = status == BatteryManager.BATTERY_STATUS_CHARGING || status == BatteryManager.BATTERY_STATUS_FULL
        return JSONObject().put("level", level * 100 / scale).put("charging", charging)
    }
}
