package dev.tether

import android.Manifest
import android.app.NotificationManager
import android.content.ComponentName
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.util.TypedValue
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.result.contract.ActivityResultContracts
import com.journeyapps.barcodescanner.ScanContract
import com.journeyapps.barcodescanner.ScanOptions

class MainActivity : ComponentActivity() {
    private val link by lazy { Link.get(this) }
    private lateinit var status: TextView
    private lateinit var setup: LinearLayout
    private lateinit var unpair: Button
    private var unobserve: (() -> Unit)? = null

    private val scan = registerForActivityResult(ScanContract()) { r -> r.contents?.let(::pair) }
    private val askNotify = registerForActivityResult(ActivityResultContracts.RequestPermission()) { refresh() }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val pad = dp(20)
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
        }
        // Edge-to-edge is enforced on recent Android; keep clear of system bars.
        root.setOnApplyWindowInsetsListener { v, insets ->
            @Suppress("DEPRECATION")
            v.setPadding(
                pad + insets.systemWindowInsetLeft, pad + insets.systemWindowInsetTop,
                pad + insets.systemWindowInsetRight, pad + insets.systemWindowInsetBottom,
            )
            insets
        }

        root.addView(TextView(this).apply {
            text = getString(R.string.app_name)
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 28f)
        })
        status = TextView(this).apply {
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 16f)
            setPadding(0, dp(8), 0, dp(16))
        }
        root.addView(status)

        root.addView(button("Scan pairing QR code") {
            scan.launch(
                ScanOptions()
                    .setDesiredBarcodeFormats(ScanOptions.QR_CODE)
                    .setPrompt("Scan the code shown by `tether pair`")
                    .setBeepEnabled(false)
                    .setOrientationLocked(false)
            )
        })
        root.addView(button("Send clipboard to desktop") {
            toast(if (Clip.sendCurrent(this)) "Clipboard sent" else "Clipboard is empty")
        })

        setup = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(0, dp(16), 0, dp(16))
        }
        root.addView(setup)

        unpair = button("Unpair") {
            link.unpair()
            stopService(Intent(this, LinkService::class.java))
        }
        root.addView(unpair)

        setContentView(ScrollView(this).apply { addView(root) })

        link.start()
        LinkService.start(this)
        handleIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleIntent(intent)
    }

    override fun onStart() {
        super.onStart()
        unobserve = link.observe { s ->
            status.text = s.describe(link.prefs.serverName)
            refresh()
        }
    }

    override fun onStop() {
        unobserve?.invoke()
        super.onStop()
    }

    override fun onResume() {
        super.onResume()
        refresh()
    }

    private fun handleIntent(intent: Intent?) {
        val data = intent?.data ?: return
        if (data.scheme == "tether") pair(data.toString())
    }

    private fun pair(uri: String) {
        status.text = "Pairing…"
        link.pair(uri) { err ->
            if (err == null) {
                toast("Paired with ${link.prefs.serverName}")
                LinkService.start(this)
            } else {
                status.text = "Pairing failed:\n$err"
            }
            refresh()
        }
    }

    /** Rebuilds the list of permissions still missing. */
    private fun refresh() {
        setup.removeAllViews()
        val nm = getSystemService(NotificationManager::class.java)
        if (!nm.isNotificationListenerAccessGranted(ComponentName(this, NotifListener::class.java))) {
            setup.addView(button("Allow reading notifications") {
                startActivity(Intent(Settings.ACTION_NOTIFICATION_LISTENER_SETTINGS))
            })
        }
        if (Build.VERSION.SDK_INT >= 33 &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            setup.addView(button("Allow showing notifications") {
                askNotify.launch(Manifest.permission.POST_NOTIFICATIONS)
            })
        }
        if (!getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)) {
            setup.addView(button("Keep running in background") {
                startActivity(
                    Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
                )
            })
        }
        if (setup.childCount > 0) {
            setup.addView(TextView(this).apply { text = "Setup needed for reliable syncing" }, 0)
        }
        unpair.visibility = if (link.prefs.server != null) View.VISIBLE else View.GONE
    }

    private fun button(label: String, onClick: () -> Unit) = Button(this).apply {
        text = label
        isAllCaps = false
        setOnClickListener { onClick() }
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()

    private fun toast(msg: String) = Toast.makeText(this, msg, Toast.LENGTH_SHORT).show()
}
