package com.monetdroid

import android.Manifest
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.view.View
import android.view.ViewGroup
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.core.app.ActivityCompat
import androidx.core.content.ContextCompat

class MainActivity : AppCompatActivity() {

    private lateinit var prefs: SharedPreferences
    private var webView: WebView? = null
    private var fileUploadCallback: ValueCallback<Array<Uri>>? = null

    companion object {
        @Volatile var isForeground = false
        private const val FILE_CHOOSER_REQUEST = 2
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = getSharedPreferences("monetdroid", MODE_PRIVATE)

        requestNotificationPermission()

        val serverUrl = prefs.getString("server_url", null)
        if (serverUrl.isNullOrBlank()) {
            showSettings()
        } else {
            val navigateUrl = intent?.getStringExtra("navigate_url") ?: serverUrl
            showWebView(navigateUrl)
        }
    }

    override fun onNewIntent(intent: Intent?) {
        super.onNewIntent(intent)
        val navigateUrl = intent?.getStringExtra("navigate_url")
        if (navigateUrl != null && webView != null) {
            webView?.loadUrl(navigateUrl)
        }
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                ActivityCompat.requestPermissions(
                    this, arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1
                )
            }
        }
    }

    private fun showSettings() {
        val layout = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(48, 120, 48, 48)
            setBackgroundColor(0xFF1a1a2e.toInt())
        }

        val title = TextView(this).apply {
            text = "Monetdroid"
            textSize = 24f
            setTextColor(0xFFBB86FC.toInt())
            setPadding(0, 0, 0, 48)
        }
        layout.addView(title)

        val label = TextView(this).apply {
            text = "Server URL"
            textSize = 14f
            setTextColor(0xFFCCCCCC.toInt())
            setPadding(0, 0, 0, 8)
        }
        layout.addView(label)

        val input = EditText(this).apply {
            setText(prefs.getString("server_url", "http://"))
            textSize = 16f
            setTextColor(0xFFFFFFFF.toInt())
            setHintTextColor(0xFF888888.toInt())
            hint = "http://192.168.1.100:8080"
            setBackgroundColor(0xFF2a2a3e.toInt())
            setPadding(24, 24, 24, 24)
            isSingleLine = true
        }
        layout.addView(input)

        val button = Button(this).apply {
            text = "Connect"
            setBackgroundColor(0xFFBB86FC.toInt())
            setTextColor(0xFF000000.toInt())
            val params = LinearLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT,
                ViewGroup.LayoutParams.WRAP_CONTENT
            )
            params.topMargin = 32
            layoutParams = params
        }
        button.setOnClickListener {
            val url = input.text.toString().trim().trimEnd('/')
            if (url.isNotBlank()) {
                prefs.edit().putString("server_url", url).apply()
                showWebView(url)
            }
        }
        layout.addView(button)

        setContentView(layout)
    }

    private fun showWebView(url: String) {
        webView = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            settings.mediaPlaybackRequiresUserGesture = false
            webViewClient = WebViewClient()
            webChromeClient = object : WebChromeClient() {
                override fun onShowFileChooser(
                    webView: WebView?,
                    callback: ValueCallback<Array<Uri>>?,
                    params: FileChooserParams?
                ): Boolean {
                    fileUploadCallback?.onReceiveValue(null)
                    fileUploadCallback = callback
                    val intent = Intent(Intent.ACTION_GET_CONTENT).apply {
                        addCategory(Intent.CATEGORY_OPENABLE)
                        type = "image/*"
                        if (params?.mode == FileChooserParams.MODE_OPEN_MULTIPLE) {
                            putExtra(Intent.EXTRA_ALLOW_MULTIPLE, true)
                        }
                    }
                    startActivityForResult(intent, FILE_CHOOSER_REQUEST)
                    return true
                }
            }
            setBackgroundColor(0xFF1a1a2e.toInt())
            loadUrl(url)
        }

        setContentView(webView, ViewGroup.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT,
            ViewGroup.LayoutParams.MATCH_PARENT
        ))

        val baseUrl = prefs.getString("server_url", null) ?: return
        startNotificationService(baseUrl)
    }

    private fun startNotificationService(serverUrl: String) {
        val intent = Intent(this, NotificationService::class.java).apply {
            putExtra("server_url", serverUrl)
        }
        ContextCompat.startForegroundService(this, intent)
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == FILE_CHOOSER_REQUEST) {
            val results = if (resultCode == RESULT_OK && data != null) {
                // Multiple files come as clipData, single file as data
                val clip = data.clipData
                if (clip != null) {
                    Array(clip.itemCount) { clip.getItemAt(it).uri }
                } else {
                    data.data?.let { arrayOf(it) }
                }
            } else null
            fileUploadCallback?.onReceiveValue(results)
            fileUploadCallback = null
        }
    }

    override fun onResume() {
        super.onResume()
        isForeground = true
    }

    override fun onPause() {
        super.onPause()
        isForeground = false
    }

    override fun onBackPressed() {
        if (webView?.canGoBack() == true) {
            webView?.goBack()
        } else {
            // Long-press back to go to settings
            showSettings()
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        webView?.destroy()
    }
}
