package io.github.carnager.tether

import android.Manifest
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.media.AudioManager
import android.net.Uri
import android.provider.ContactsContract
import android.telecom.TelecomManager
import android.telephony.TelephonyManager
import android.util.Log
import org.json.JSONObject

/**
 * Reports phone calls to the computers, which pause media or lower the
 * volume and show the caller.
 */
object Calls {
    private const val TAG = "tether"

    /** Permissions needed to see calls and who is calling. */
    val permissions = arrayOf(
        Manifest.permission.READ_PHONE_STATE,
        Manifest.permission.READ_CALL_LOG,
        Manifest.permission.READ_CONTACTS,
    )

    // The call's progress is kept on disk: Android may kill and restart the
    // app between "ringing" and "ended", and a lost "ended" would leave the
    // computer's media paused.
    private fun store(ctx: Context) = ctx.getSharedPreferences("calls", Context.MODE_PRIVATE)
    private var Context.lastState: String
        get() = store(this).getString("state", null) ?: TelephonyManager.EXTRA_STATE_IDLE
        set(v) = store(this).edit().putString("state", v).commit().let {}
    private var Context.number: String?
        get() = store(this).getString("number", null)
        set(v) = store(this).edit().putString("number", v).commit().let {}
    private var Context.mutedRinger: Boolean
        get() = store(this).getBoolean("muted", false)
        set(v) = store(this).edit().putBoolean("muted", v).commit().let {}

    fun enabled(ctx: Context) =
        ctx.checkSelfPermission(Manifest.permission.READ_PHONE_STATE) == PackageManager.PERMISSION_GRANTED

    @Synchronized
    fun onPhoneState(ctx: Context, state: String, incoming: String?) = with(ctx) {
        if (incoming != null) number = incoming
        val event = when (state) {
            TelephonyManager.EXTRA_STATE_RINGING -> "ringing"
            TelephonyManager.EXTRA_STATE_OFFHOOK -> "talking"
            else -> "ended"
        }
        // The same state is broadcast twice (with and without the number);
        // only resend it if we learned the number.
        if (state == lastState && incoming == null) return@with
        if (state == TelephonyManager.EXTRA_STATE_IDLE && lastState == TelephonyManager.EXTRA_STATE_IDLE) return@with
        lastState = state

        val body = JSONObject().put("event", event).put("time", System.currentTimeMillis())
        number?.let { n ->
            body.put("number", n)
            contactName(ctx, n)?.let { body.put("name", it) }
        }
        val links = Links.get(ctx)
        links.start()
        links.send("call.state", body)

        if (event == "ended") {
            number = null
            restoreRinger(ctx)
        }
    }

    /** Silences the ringer for the current call, on request from a computer. */
    @Synchronized
    fun muteRinger(ctx: Context) = with(ctx) {
        if (lastState != TelephonyManager.EXTRA_STATE_RINGING) return@with
        try {
            ctx.getSystemService(TelecomManager::class.java).silenceRinger()
            return@with
        } catch (e: SecurityException) {
            // Only the default dialer may use silenceRinger on recent Android.
        }
        try {
            ctx.getSystemService(AudioManager::class.java)
                .adjustStreamVolume(AudioManager.STREAM_RING, AudioManager.ADJUST_MUTE, 0)
            mutedRinger = true
        } catch (e: SecurityException) {
            Log.w(TAG, "could not mute the ringer", e)
        }
    }

    private fun restoreRinger(ctx: Context) = with(ctx) {
        if (!mutedRinger) return@with
        mutedRinger = false
        try {
            ctx.getSystemService(AudioManager::class.java)
                .adjustStreamVolume(AudioManager.STREAM_RING, AudioManager.ADJUST_UNMUTE, 0)
        } catch (e: SecurityException) {
            Log.w(TAG, "could not unmute the ringer", e)
        }
    }

    private fun contactName(ctx: Context, number: String): String? {
        if (ctx.checkSelfPermission(Manifest.permission.READ_CONTACTS) != PackageManager.PERMISSION_GRANTED) return null
        val uri = Uri.withAppendedPath(ContactsContract.PhoneLookup.CONTENT_FILTER_URI, Uri.encode(number))
        return try {
            ctx.contentResolver.query(uri, arrayOf(ContactsContract.PhoneLookup.DISPLAY_NAME), null, null, null)?.use { c ->
                if (c.moveToFirst()) c.getString(0) else null
            }
        } catch (e: Exception) {
            Log.w(TAG, "contact lookup failed", e)
            null
        }
    }
}

/** Receives call state changes, even when the app isn't running. */
class CallReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != TelephonyManager.ACTION_PHONE_STATE_CHANGED || !Calls.enabled(ctx)) return
        val state = intent.getStringExtra(TelephonyManager.EXTRA_STATE) ?: return
        @Suppress("DEPRECATION")
        val number = intent.getStringExtra(TelephonyManager.EXTRA_INCOMING_NUMBER)
        Calls.onPhoneState(ctx, state, number)
    }
}
