package io.github.carnager.tether

import android.app.Activity
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Computer
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.ListItem
import androidx.compose.material3.ListItemDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import android.app.Application
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.graphics.drawable.Icon
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.service.notification.NotificationListenerService
import android.service.notification.StatusBarNotification
import android.service.quicksettings.TileService
import android.util.Log
import android.widget.Toast
import org.json.JSONObject

private const val TAG = "tether"

class App : Application() {
    override fun onCreate() {
        super.onCreate()
        Notifs.setup(this)
    }
}

object Notifs {
    const val LINK = "link"
    const val TRANSFERS = "transfers"
    const val FILES = "files"
    const val MEDIA = "media"
    const val LINK_ID = 1

    fun setup(ctx: Context) {
        ctx.getSystemService(NotificationManager::class.java).createNotificationChannels(
            listOf(
                NotificationChannel(LINK, "Connection status", NotificationManager.IMPORTANCE_MIN),
                NotificationChannel(TRANSFERS, "Transfer progress", NotificationManager.IMPORTANCE_LOW),
                NotificationChannel(FILES, "Finished transfers", NotificationManager.IMPORTANCE_DEFAULT),
                NotificationChannel(MEDIA, "Media remote", NotificationManager.IMPORTANCE_LOW),
            )
        )
    }
}

object Clip {
    /** Sets the phone clipboard. Apps may write it from the background. */
    fun set(ctx: Context, text: String) = Handler(Looper.getMainLooper()).post {
        ctx.getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("tether", text))
    }

    /**
     * Sends the phone clipboard to the desktop. Android only allows reading
     * it while one of our windows has focus.
     */
    fun sendCurrent(ctx: Context): Boolean {
        val clip = ctx.getSystemService(ClipboardManager::class.java).primaryClip
        val text = clip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.coerceToText(ctx)?.toString()
        if (text.isNullOrEmpty()) return false
        Links.get(ctx).send("clip.set", JSONObject().put("text", text))
        return true
    }
}

/**
 * Keeps the process, and with it the connection, alive. Its notification
 * shows the connection state and offers "Send clipboard".
 */
class LinkService : Service() {
    private var unobserve: (() -> Unit)? = null

    override fun onBind(intent: Intent?) = null

    override fun onCreate() {
        super.onCreate()
        val links = Links.get(this)
        startForeground(Notifs.LINK_ID, build(), ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE)
        unobserve = links.observe {
            getSystemService(NotificationManager::class.java).notify(Notifs.LINK_ID, build())
        }
        links.start()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_SHARE) {
            // The URI read grants came along with this intent's ClipData.
            val clip = intent.clipData
            val uris = if (clip == null) emptyList() else (0 until clip.itemCount).mapNotNull { clip.getItemAt(it).uri }
            val links = Links.get(this)
            for (id in intent.getStringArrayExtra(EXTRA_TARGETS).orEmpty()) links.get(id)?.transfers?.share(uris)
        }
        return START_STICKY
    }

    override fun onDestroy() {
        unobserve?.invoke()
        super.onDestroy()
    }

    private fun build(): Notification {
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val clip = PendingIntent.getActivity(
            this, 1,
            Intent(this, ClipActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_IMMUTABLE,
        )
        return Notification.Builder(this, Notifs.LINK)
            .setSmallIcon(R.drawable.ic_stat)
            .setContentTitle(summary(Links.get(this).all()))
            .setContentIntent(open)
            .setOngoing(true)
            .setShowWhen(false)
            .addAction(Notification.Action.Builder(Icon.createWithResource(this, R.drawable.ic_stat), "Send clipboard", clip).build())
            .build()
    }

    private fun summary(links: List<Link>): String {
        val connected = links.filter { it.status == Status.CONNECTED }.map { it.name }
        return when {
            links.isEmpty() -> "Not paired"
            links.size == 1 -> "${links[0].name}: ${links[0].status.describe()}"
            connected.isEmpty() -> "No computer connected"
            else -> "Connected to ${connected.joinToString(", ")}"
        }
    }

    companion object {
        const val ACTION_SHARE = "io.github.carnager.tether.SHARE"
        const val EXTRA_TARGETS = "io.github.carnager.tether.TARGETS"

        fun start(ctx: Context) {
            if (!Links.get(ctx).isPaired) return
            try {
                ctx.startForegroundService(Intent(ctx, LinkService::class.java))
            } catch (e: Exception) {
                // Background start restrictions; the notification listener
                // keeps the link running in the meantime.
                Log.w(TAG, "could not start service", e)
            }
        }
    }
}

/**
 * Mirrors notifications to the desktop. The system keeps this service bound
 * as long as notification access is granted, which makes it a reliable
 * anchor for the whole process.
 */
class NotifListener : NotificationListenerService() {
    private val sent = HashMap<String, Int>() // notification key -> hash of what we sent

    override fun onListenerConnected() {
        Links.get(this).start()
        LinkService.start(this)
    }

    override fun onNotificationPosted(sbn: StatusBarNotification) {
        if (sbn.packageName == packageName || !sbn.isClearable) return
        val n = sbn.notification
        val skipFlags = Notification.FLAG_ONGOING_EVENT or Notification.FLAG_GROUP_SUMMARY or
            Notification.FLAG_FOREGROUND_SERVICE or Notification.FLAG_LOCAL_ONLY
        if (n.flags and skipFlags != 0) return
        if (n.category in setOf(Notification.CATEGORY_PROGRESS, Notification.CATEGORY_TRANSPORT, Notification.CATEGORY_SERVICE)) return

        val ex = n.extras
        val title = ex.getCharSequence(Notification.EXTRA_TITLE)?.toString().orEmpty()
        val text = (ex.getCharSequence(Notification.EXTRA_BIG_TEXT) ?: ex.getCharSequence(Notification.EXTRA_TEXT))
            ?.toString().orEmpty()
        if (title.isEmpty() && text.isEmpty()) return

        val links = Links.get(this)
        val label = appLabel(sbn.packageName)
        links.prefs.addSeenApp(sbn.packageName, label)
        if (links.prefs.isMuted(sbn.packageName)) return

        // Apps re-post unchanged notifications a lot; only send real changes.
        val hash = "$title\u0000$text".hashCode()
        if (sent[sbn.key] == hash) return
        sent[sbn.key] = hash

        links.send(
            "notif.posted",
            JSONObject()
                .put("key", sbn.key)
                .put("app", sbn.packageName)
                .put("app_name", label)
                .put("title", title)
                .put("text", text)
                .put("time", sbn.postTime),
        )
    }

    override fun onNotificationRemoved(sbn: StatusBarNotification) {
        if (sent.remove(sbn.key) != null) {
            Links.get(this).send("notif.removed", JSONObject().put("key", sbn.key))
        }
    }

    private fun appLabel(pkg: String): String = try {
        packageManager.getApplicationLabel(packageManager.getApplicationInfo(pkg, 0)).toString()
    } catch (_: Exception) {
        pkg
    }
}

/**
 * Share-sheet target: files are uploaded, plain text goes to the computer's
 * clipboard. With several computers paired it asks which one.
 */
class ShareActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val links = Links.get(this).all()
        when {
            links.isEmpty() -> {
                toast("Pair with a computer first")
                finish()
            }
            links.size == 1 -> {
                send(links)
                finish()
            }
            else -> setContent {
                TetherTheme {
                    AlertDialog(
                        onDismissRequest = ::finish,
                        title = { Text("Send to") },
                        text = {
                            Column {
                                for (link in links) {
                                    ListItem(
                                        modifier = Modifier.clickable { send(listOf(link)); finish() },
                                        headlineContent = { Text(link.name) },
                                        supportingContent = { Text(link.status.describe()) },
                                        leadingContent = { Icon(Icons.Rounded.Computer, null) },
                                        colors = ListItemDefaults.colors(containerColor = Color.Transparent),
                                    )
                                }
                            }
                        },
                        confirmButton = { TextButton(onClick = { send(links); finish() }) { Text("All computers") } },
                        dismissButton = { TextButton(onClick = ::finish) { Text("Cancel") } },
                    )
                }
            }
        }
    }

    private fun send(targets: List<Link>) {
        val uris = sharedUris()
        val text = intent.getStringExtra(Intent.EXTRA_TEXT)
        val where = if (targets.size == 1) targets[0].name else "${targets.size} computers"
        when {
            uris.isNotEmpty() -> {
                val svc = Intent(this, LinkService::class.java)
                    .setAction(LinkService.ACTION_SHARE)
                    .putExtra(LinkService.EXTRA_TARGETS, targets.map { it.id }.toTypedArray())
                    .addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
                svc.clipData = ClipData.newRawUri("", uris[0]).apply { uris.drop(1).forEach { addItem(ClipData.Item(it)) } }
                startForegroundService(svc)
                toast(if (uris.size == 1) "Sending to $where" else "Sending ${uris.size} files to $where")
            }
            text != null -> {
                Links.get(this).start()
                targets.forEach { it.send("clip.set", JSONObject().put("text", text)) }
                toast("Copied to the clipboard on $where")
            }
            else -> toast("Nothing to send")
        }
    }

    @Suppress("DEPRECATION")
    private fun sharedUris(): List<Uri> = when (intent.action) {
        Intent.ACTION_SEND -> listOfNotNull(
            if (Build.VERSION.SDK_INT >= 33) intent.getParcelableExtra(Intent.EXTRA_STREAM, Uri::class.java)
            else intent.getParcelableExtra(Intent.EXTRA_STREAM)
        )
        Intent.ACTION_SEND_MULTIPLE -> (
            if (Build.VERSION.SDK_INT >= 33) intent.getParcelableArrayListExtra(Intent.EXTRA_STREAM, Uri::class.java)
            else intent.getParcelableArrayListExtra(Intent.EXTRA_STREAM)
            ).orEmpty()
        else -> emptyList()
    }

    private fun toast(msg: String) = Toast.makeText(applicationContext, msg, Toast.LENGTH_SHORT).show()
}

/** Forwards a `tether://pair` link to the main screen. */
class PairLinkActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        startActivity(
            Intent(this, MainActivity::class.java)
                .setData(intent.data)
                // CLEAR_TOP + SINGLE_TOP delivers to an existing MainActivity via onNewIntent.
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP)
        )
        finish()
    }
}

/** Invisible activity that exists only to gain focus, read the clipboard and send it. */
class ClipActivity : Activity() {
    private var done = false

    override fun onWindowFocusChanged(hasFocus: Boolean) {
        super.onWindowFocusChanged(hasFocus)
        if (!hasFocus || done) return
        done = true
        val ok = Clip.sendCurrent(this)
        Toast.makeText(applicationContext, if (ok) "Clipboard sent" else "Clipboard is empty", Toast.LENGTH_SHORT).show()
        finish()
    }
}

class ClipTile : TileService() {
    override fun onClick() {
        val intent = Intent(this, ClipActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (Build.VERSION.SDK_INT >= 34) {
            startActivityAndCollapse(PendingIntent.getActivity(this, 0, intent, PendingIntent.FLAG_IMMUTABLE))
        } else {
            @Suppress("DEPRECATION")
            startActivityAndCollapse(intent)
        }
    }
}

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        LinkService.start(ctx)
    }
}
