package dev.tether

import android.app.Activity
import android.os.Bundle
import android.util.TypedValue
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.Switch
import android.widget.TextView

/** Choose which apps' notifications are forwarded to the desktop. */
class AppsActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        title = "Forwarded notifications"
        val prefs = Link.get(this).prefs
        val pad = (20 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
        }
        root.setOnApplyWindowInsetsListener { v, insets ->
            @Suppress("DEPRECATION")
            v.setPadding(
                pad + insets.systemWindowInsetLeft, pad + insets.systemWindowInsetTop,
                pad + insets.systemWindowInsetRight, pad + insets.systemWindowInsetBottom,
            )
            insets
        }

        val apps = prefs.seenApps().entries.sortedBy { it.value.lowercase() }
        root.addView(TextView(this).apply {
            setTextSize(TypedValue.COMPLEX_UNIT_SP, 16f)
            setPadding(0, 0, 0, pad / 2)
            text = if (apps.isEmpty()) {
                "Apps appear here once they have shown a notification."
            } else {
                "Apps that have shown notifications. Turn one off to stop forwarding it."
            }
        })
        for ((pkg, label) in apps) {
            root.addView(Switch(this).apply {
                text = label
                setTextSize(TypedValue.COMPLEX_UNIT_SP, 16f)
                setPadding(0, pad / 3, 0, pad / 3)
                isChecked = !prefs.isMuted(pkg)
                setOnCheckedChangeListener { _, on -> prefs.setMuted(pkg, !on) }
            })
        }
        setContentView(ScrollView(this).apply { addView(root) })
    }
}
