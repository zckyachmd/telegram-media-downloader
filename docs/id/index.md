---
title: Panduan Pengguna Bahasa Indonesia
description: Panduan memulai, mengunduh media Telegram, mengelola akun, dan menjaga data Telegram Media Downloader.
lang: id
permalink: /id/
nav_order: 1
---

# Telegram Media Downloader

Simpan foto, video, dan file dari chat Telegram ke server Anda sendiri. Aplikasi ini menyediakan dashboard web, pemantauan otomatis, antrean unduhan, serta pencadangan.

Bahasa: [English]({{ '/' | relative_url }}) · [ไทย]({{ '/th/' | relative_url }}) · **Indonesia**

## Mulai dengan Docker Compose

Siapkan Docker dengan Compose. Buat folder data dan jalankan aplikasi:

```sh
mkdir telegram-media-downloader && cd telegram-media-downloader
mkdir -p data
curl -fsSLO https://raw.githubusercontent.com/botnick/telegram-media-downloader/main/docker-compose.yml
docker compose up -d
```

Buka `http://localhost:3000`. Pada penggunaan pertama, buat kata sandi dashboard. Jika server berjalan di mesin lain, ganti `localhost` dengan alamat server.

## Hubungkan akun Telegram

1. Buat `apiId` dan `apiHash` di [my.telegram.org](https://my.telegram.org).
2. Di dashboard, buka **Pengaturan → Telegram API** dan masukkan kedua nilai tersebut.
3. Buka **Tambah Akun**, masukkan nomor telepon beserta kode negara, lalu verifikasi kode Telegram. Masukkan kata sandi 2FA jika diminta.
4. Pilih chat yang ingin dipantau. Postingan baru dari chat tersebut akan masuk ke antrean unduhan.

## Unduh riwayat atau satu pesan

- Untuk mengambil pesan lama, buka **Backfill** dari halaman chat dan pilih batas pesan atau rentang tanggal.
- Untuk mengunduh satu pesan, tempel tautan `t.me` di bilah atas dashboard.
- Pantau progres dan batalkan pekerjaan dari **Antrean**. File yang selesai muncul di galeri chat dan **Pustaka**.

## Atur ruang penyimpanan

Di **Pengaturan → Unduhan**, atur batas ukuran file, jumlah unduhan bersamaan, dan kecepatan maksimum. Pengaturan **Rotasi Disk** dapat menghapus unduhan terlama saat batas disk tercapai. Item yang disematkan dan dipulihkan tetap disimpan.

Untuk membersihkan duplikat, buka **Pemeliharaan → Duplikat**. Tinjau salinan yang dipilih sebelum menghapusnya.

## Amankan dan cadangkan data

- Gunakan kata sandi dashboard yang kuat. Jangan publikasikan port dashboard tanpa HTTPS dan pengaturan akses yang sesuai.
- Sesi Telegram disimpan terenkripsi di folder data aplikasi. Jangan membagikan folder data atau token sesi.
- Cadangkan folder `data/` secara berkala. Untuk tujuan jarak jauh, buka **Pemeliharaan → Pencadangan** dan tambahkan disk, S3, SFTP, Google Drive, atau Dropbox.
- Jika mengaktifkan enkripsi pencadangan, simpan frasa sandinya dengan aman. Frasa sandi yang hilang tidak dapat dipulihkan.

## Perbarui aplikasi

Di dashboard, buka **Pemeliharaan → Pembaruan** lalu pilih **Instal Pembaruan**. Aplikasi membuat snapshot database terlebih dahulu. Anda juga dapat memperbarui dari terminal dengan `docker compose pull && docker compose up -d`.

## Dokumentasi teknis

Dokumentasi API, arsitektur, catatan developer, dan referensi konfigurasi tingkat lanjut saat ini tersedia dalam bahasa Inggris di [daftar dokumentasi]({{ '/docs/' | relative_url }}).

## Bahasa dashboard

Pilih **Indonesia** di **Pengaturan → Bahasa**. Pilihan **Auto** mengikuti bahasa browser yang didukung; bahasa Inggris digunakan jika bahasa browser tidak tersedia.
