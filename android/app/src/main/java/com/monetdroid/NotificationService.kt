package com.monetdroid

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.os.IBinder
import androidx.core.app.NotificationCompat
import org.json.JSONObject
import java.io.BufferedReader
import java.io.InputStreamReader
import java.net.HttpURLConnection
import java.net.URL

class NotificationService : Service() {

    private var serverUrl: String = ""
    private var sseThread: Thread? = null
    @Volatile private var running = false
    @Volatile private var activeConnection: HttpURLConnection? = null
    private var notificationId = 100
    private val threadLock = Any()  // Guards thread restart

    companion object {
        const val CHANNEL_FOREGROUND = "monetdroid_foreground"
        const val CHANNEL_ALERTS = "monetdroid_alerts"
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        createChannels()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        serverUrl = intent?.getStringExtra("server_url") ?: return START_NOT_STICKY

        startForeground(1, buildForegroundNotification())

        synchronized(threadLock) {
            // Stop old thread — close the connection to unblock readLine()
            running = false
            activeConnection?.disconnect()
            sseThread?.interrupt()
            sseThread?.let {
                try {
                    it.join(2000)
                } catch (e: InterruptedException) {
                    // Thread was interrupted during join, continue
                }
            }

            // Start new thread
            running = true
            sseThread = Thread { connectSSE(serverUrl) }.apply {
                isDaemon = true
                start()
            }
        }

        return START_STICKY
    }

    override fun onDestroy() {
        synchronized(threadLock) {
            running = false
            activeConnection?.disconnect()
            sseThread?.interrupt()
            sseThread = null
        }
        super.onDestroy()
    }

    private fun createChannels() {
        val manager = getSystemService(NotificationManager::class.java)

        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_FOREGROUND, "Background Connection",
                NotificationManager.IMPORTANCE_LOW
            ).apply { description = "Keeps the connection to the server alive" }
        )

        manager.createNotificationChannel(
            NotificationChannel(
                CHANNEL_ALERTS, "Alerts",
                NotificationManager.IMPORTANCE_HIGH
            ).apply {
                description = "Permission prompts and task completion"
                enableVibration(true)
            }
        )
    }

    private fun buildForegroundNotification(): Notification {
        val intent = Intent(this, MainActivity::class.java)
        val pending = PendingIntent.getActivity(
            this, 0, intent, PendingIntent.FLAG_IMMUTABLE
        )
        return NotificationCompat.Builder(this, CHANNEL_FOREGROUND)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle("Monetdroid")
            .setContentText("Connected")
            .setContentIntent(pending)
            .setOngoing(true)
            .build()
    }

    private fun connectSSE(serverUrl: String) {
        while (running) {
            var conn: HttpURLConnection? = null
            try {
                val url = URL("$serverUrl/api/notifications")
                conn = url.openConnection() as HttpURLConnection
                conn.setRequestProperty("Accept", "text/event-stream")
                conn.connectTimeout = 10_000
                conn.readTimeout = 60_000 // safety net; server sends heartbeats every 30s
                activeConnection = conn

                val reader = BufferedReader(InputStreamReader(conn.inputStream))
                var eventType = ""
                val data = StringBuilder()

                while (running) {
                    val line = reader.readLine() ?: break
                    when {
                        line.startsWith("event:") -> {
                            eventType = line.removePrefix("event:").trim()
                        }
                        line.startsWith("data:") -> {
                            data.append(line.removePrefix("data:").trim())
                        }
                        line.isEmpty() && data.isNotEmpty() -> {
                            handleEvent(eventType, data.toString())
                            eventType = ""
                            data.clear()
                        }
                    }
                }
                reader.close()
            } catch (e: InterruptedException) {
                break
            } catch (e: Exception) {
                // Connection failed, dropped, or disconnected — retry after delay
                if (running) {
                    try { Thread.sleep(5_000) } catch (_: InterruptedException) { break }
                }
            } finally {
                activeConnection = null
                conn?.disconnect()
            }
        }
    }

    private fun handleEvent(eventType: String, data: String) {
        val json = try { JSONObject(data) } catch (_: Exception) { return }
        val text = json.optString("text", "")
        val session = json.optString("session", "")
        val cwd = json.optString("cwd", "")

        when (eventType) {
            "permission" -> {
                val body = if (cwd.isNotEmpty()) "$cwd: $text" else text
                postAlert("Permission Required", body, session)
            }
            "done" -> {
                val body = if (cwd.isNotEmpty()) cwd else "Task complete"
                postAlert("Task Complete", body, session)
            }
        }
    }

    private fun postAlert(title: String, text: String, session: String) {
        if (MainActivity.isForeground) return

        val sessionUrl = if (session.isNotEmpty()) {
            "$serverUrl/?session=$session"
        } else {
            serverUrl
        }

        val intent = Intent(this, MainActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP
            putExtra("navigate_url", sessionUrl)
        }
        val pending = PendingIntent.getActivity(
            this, notificationId, intent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        val notification = NotificationCompat.Builder(this, CHANNEL_ALERTS)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setContentIntent(pending)
            .setAutoCancel(true)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .build()

        val manager = getSystemService(NotificationManager::class.java)
        manager.notify(notificationId++, notification)
    }
}
